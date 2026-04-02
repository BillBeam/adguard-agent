package compact

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// Compression strategy constants.

// CompactConfig controls ContextManager behavior.
type CompactConfig struct {
	// ContextWindowSize is the model's context window in tokens.
	// Default: 131072 (128K, conservative for grok-4-1-fast-reasoning).
	ContextWindowSize int

	// AutoCompactBuffer is the buffer before the context limit that triggers AutoCompact.
	// Trigger threshold = ContextWindowSize - SummaryOutputReserve - AutoCompactBuffer.
	// Default: 13000 (empirical value balancing compression frequency vs context utilization).
	AutoCompactBuffer int

	// SummaryOutputReserve is the max_tokens allocated for the summarization LLM call.
	// Default: 8000 (ad review summaries are shorter than general conversations).
	SummaryOutputReserve int

	// MicroCompactKeepRecent preserves the N most recent tool_result messages from clearing.
	// Default: 6 (current ad's tool calls are typically 3-5, keep a buffer).
	MicroCompactKeepRecent int

	// MaxConsecutiveFailures is the circuit breaker limit.
	// After this many consecutive AutoCompact failures, stop retrying.
	// Default: 3 (matches reference MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES).
	MaxConsecutiveFailures int
}

// DefaultCompactConfig returns production defaults.
func DefaultCompactConfig() CompactConfig {
	return CompactConfig{
		ContextWindowSize:      131072,
		AutoCompactBuffer:      13000,
		SummaryOutputReserve:   8000,
		MicroCompactKeepRecent: 6,
		MaxConsecutiveFailures: 3,
	}
}

// CompactResult describes the outcome of a compression operation.
type CompactResult struct {
	Compacted    bool
	Strategy     string // "micro", "auto", "reactive", ""
	TokensBefore int
	TokensAfter  int
	Messages     []types.Message
	Error        error
}

// CompactState tracks cross-turn compression state.
type CompactState struct {
	ConsecutiveFailures int
	HasCompacted        bool
	CompactCount        int
}

// microCompactPlaceholder replaces cleared tool result content.
const microCompactPlaceholder = "[旧工具结果已清除]"

// ContextManager handles conversation context lifecycle.
// Implements a three-layer cascading compression strategy.
type ContextManager struct {
	config CompactConfig
	client llm.LLMClient
	state  CompactState
	logger *slog.Logger
}

// NewContextManager creates a ContextManager.
// client can be nil if only MicroCompact (no LLM) is needed.
func NewContextManager(config CompactConfig, client llm.LLMClient, logger *slog.Logger) *ContextManager {
	return &ContextManager{
		config: config,
		client: client,
		logger: logger,
	}
}

// autoCompactThreshold calculates the token count threshold for triggering AutoCompact.
func (cm *ContextManager) autoCompactThreshold() int {
	return cm.config.ContextWindowSize - cm.config.SummaryOutputReserve - cm.config.AutoCompactBuffer
}

// PreRequest executes proactive compression before each API call.
//
// Strategy chain: MicroCompact → check threshold → AutoCompact.
// Called from loop.go before buildRequest().
func (cm *ContextManager) PreRequest(ctx context.Context, messages []types.Message) CompactResult {
	tokensBefore := EstimateMessagesTokens(messages)

	// L1: MicroCompact — always run, zero LLM cost.
	messages = cm.microCompact(messages)
	tokensAfterMicro := EstimateMessagesTokens(messages)

	microSaved := tokensBefore - tokensAfterMicro
	if microSaved > 0 {
		cm.logger.Debug("micro compact",
			slog.Int("tokens_freed", microSaved),
		)
	}

	// L2: AutoCompact — only if still above threshold.
	threshold := cm.autoCompactThreshold()
	if tokensAfterMicro < threshold {
		// Below threshold: return micro-compacted messages.
		if microSaved > 0 {
			return CompactResult{
				Compacted:    true,
				Strategy:     "micro",
				TokensBefore: tokensBefore,
				TokensAfter:  tokensAfterMicro,
				Messages:     messages,
			}
		}
		return CompactResult{Messages: messages}
	}

	// Circuit breaker check.
	if cm.state.ConsecutiveFailures >= cm.config.MaxConsecutiveFailures {
		cm.logger.Warn("auto compact circuit breaker tripped",
			slog.Int("failures", cm.state.ConsecutiveFailures),
		)
		return CompactResult{
			Compacted:    microSaved > 0,
			Strategy:     "micro",
			TokensBefore: tokensBefore,
			TokensAfter:  tokensAfterMicro,
			Messages:     messages,
		}
	}

	// Execute AutoCompact.
	compacted, err := cm.autoCompact(ctx, messages)
	if err != nil {
		cm.state.ConsecutiveFailures++
		cm.logger.Error("auto compact failed",
			slog.String("error", err.Error()),
			slog.Int("failures", cm.state.ConsecutiveFailures),
		)
		return CompactResult{
			Compacted:    microSaved > 0,
			Strategy:     "micro",
			TokensBefore: tokensBefore,
			TokensAfter:  tokensAfterMicro,
			Messages:     messages,
			Error:        err,
		}
	}

	cm.state.ConsecutiveFailures = 0
	cm.state.HasCompacted = true
	cm.state.CompactCount++
	tokensAfter := EstimateMessagesTokens(compacted)

	cm.logger.Info("auto compact succeeded",
		slog.Int("before", tokensAfterMicro),
		slog.Int("after", tokensAfter),
		slog.Int("compact_count", cm.state.CompactCount),
	)

	return CompactResult{
		Compacted:    true,
		Strategy:     "auto",
		TokensBefore: tokensBefore,
		TokensAfter:  tokensAfter,
		Messages:     compacted,
	}
}

// ReactiveCompact executes compression after a prompt_too_long error.
// Called from loop.go when isPromptTooLongError is detected.
func (cm *ContextManager) ReactiveCompact(ctx context.Context, messages []types.Message) CompactResult {
	tokensBefore := EstimateMessagesTokens(messages)

	// Circuit breaker.
	if cm.state.ConsecutiveFailures >= cm.config.MaxConsecutiveFailures {
		return CompactResult{Messages: messages, Error: fmt.Errorf("circuit breaker: %d failures", cm.state.ConsecutiveFailures)}
	}

	// MicroCompact first, then force AutoCompact regardless of threshold.
	messages = cm.microCompact(messages)

	compacted, err := cm.autoCompact(ctx, messages)
	if err != nil {
		cm.state.ConsecutiveFailures++
		return CompactResult{Messages: messages, Error: err}
	}

	cm.state.ConsecutiveFailures = 0
	cm.state.HasCompacted = true
	cm.state.CompactCount++
	tokensAfter := EstimateMessagesTokens(compacted)

	cm.logger.Info("reactive compact succeeded",
		slog.Int("before", tokensBefore),
		slog.Int("after", tokensAfter),
	)

	return CompactResult{
		Compacted:    true,
		Strategy:     "reactive",
		TokensBefore: tokensBefore,
		TokensAfter:  tokensAfter,
		Messages:     compacted,
	}
}

// microCompact replaces old tool_result content with a placeholder.
// Keeps the most recent MicroCompactKeepRecent tool results intact.
//
// Why replace instead of delete: OpenAI API requires tool_result messages
// to pair with their tool_call — deleting a tool_result causes 400 errors.
func (cm *ContextManager) microCompact(messages []types.Message) []types.Message {
	// Count total tool messages to determine which to keep.
	toolIndices := make([]int, 0, len(messages))
	for i, msg := range messages {
		if msg.Role == types.RoleTool {
			toolIndices = append(toolIndices, i)
		}
	}

	// Nothing to compact if we have fewer tool messages than the keep limit.
	keepCount := cm.config.MicroCompactKeepRecent
	if len(toolIndices) <= keepCount {
		return messages
	}

	// Indices to clear: all tool messages except the last keepCount.
	clearUpTo := len(toolIndices) - keepCount
	clearSet := make(map[int]bool, clearUpTo)
	for i := 0; i < clearUpTo; i++ {
		clearSet[toolIndices[i]] = true
	}

	// Build new slice with cleared content. Don't modify original.
	result := make([]types.Message, len(messages))
	copy(result, messages)
	for idx := range clearSet {
		result[idx] = types.Message{
			Role:       types.RoleTool,
			Content:    types.NewTextContent(microCompactPlaceholder),
			ToolCallID: messages[idx].ToolCallID,
		}
	}
	return result
}

// autoCompact uses LLM to summarize the conversation history.
//
// Flow:
//  1. Extract system prompt (messages[0])
//  2. Build summary request: all messages + compact prompt
//  3. Call LLM (no tools, MaxTokens = SummaryOutputReserve)
//  4. Extract <summary> via FormatCompactSummary
//  5. Build new messages: [system_prompt, summary_user_msg]
func (cm *ContextManager) autoCompact(ctx context.Context, messages []types.Message) ([]types.Message, error) {
	if cm.client == nil {
		return nil, fmt.Errorf("no LLM client available for auto compact")
	}

	if len(messages) < 2 {
		return nil, fmt.Errorf("too few messages to compact: %d", len(messages))
	}

	// Preserve the system prompt.
	systemMsg := messages[0]

	// Build the summary request: conversation + compact prompt.
	summaryMessages := make([]types.Message, len(messages))
	copy(summaryMessages, messages)
	summaryMessages = append(summaryMessages, types.Message{
		Role:    types.RoleUser,
		Content: types.NewTextContent(BuildCompactPrompt()),
	})

	maxTokens := cm.config.SummaryOutputReserve
	resp, err := cm.client.ChatCompletion(ctx, types.ChatCompletionRequest{
		Messages:  summaryMessages,
		MaxTokens: &maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("compact LLM call: %w", err)
	}
	if resp == nil || len(resp.Choices) == 0 {
		return nil, fmt.Errorf("compact LLM returned empty response")
	}

	rawSummary := resp.Choices[0].Message.Content.String()
	summary := FormatCompactSummary(rawSummary)

	if len(summary) < 50 {
		return nil, fmt.Errorf("compact summary too short (%d chars), likely failed", len(summary))
	}

	// Build compressed messages: system prompt + summary.
	return []types.Message{
		systemMsg,
		{
			Role:    types.RoleUser,
			Content: types.NewTextContent(BuildCompactSummaryMessage(summary)),
		},
	}, nil
}
