package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"io"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// ExitReason describes why the agentic loop terminated.
type ExitReason string

const (
	ExitCompleted     ExitReason = "completed"       // normal: LLM output stop, no tool calls
	ExitMaxTurns      ExitReason = "max_turns"       // hit turn limit
	ExitAborted       ExitReason = "aborted"         // context cancelled
	ExitModelError    ExitReason = "model_error"     // unrecoverable API error
	ExitPromptTooLong ExitReason = "prompt_too_long" // prompt too long after recovery attempts
)

// LoopResult is the final output of the agentic loop.
type LoopResult struct {
	ExitReason       ExitReason          `json:"exit_reason"`
	ReviewResult     *types.ReviewResult `json:"review_result,omitempty"`
	MultiAgentDetail *MultiAgentResult   `json:"multi_agent_detail,omitempty"` // populated for multi-agent reviews
	State            *State              `json:"-"`
	Error            error               `json:"-"`
}

// Run executes the Agentic Loop — the core review lifecycle state machine.
//
// Structure:
//
//	for turnCount < maxTurns:
//	  1. Check context cancellation
//	  2. Build API request (messages + tools)
//	  3. Call LLM API
//	  4. Parse finish_reason:
//	     - "stop"       → parse ReviewResult → exit
//	     - "tool_calls" → execute tools → append results → continue
//	     - "length"     → max_output_tokens recovery → continue or fallback
//	  5. Emit StreamEvents via channel
//
// The events channel is optional (nil-safe). Pass nil for silent operation.
func Run(
	ctx context.Context,
	client llm.LLMClient,
	config *LoopConfig,
	state *State,
	events chan<- StreamEvent,
	logger *slog.Logger,
) (loopResult *LoopResult) {
	// Phase 5: StopHook runs on ALL exit paths via defer+named return.
	defer func() {
		if loopResult != nil && len(config.StopHooks) > 0 {
			runStopHooks(config.StopHooks, state, loopResult.ExitReason, logger)
		}
	}()

	state.Transition(StateAnalyzing, TransitionInitialized, "loop started")
	emitEvent(events, EventTurnStarted, state, "")

	for state.TurnCount < config.MaxTurns {
		// 1. Check context cancellation.
		select {
		case <-ctx.Done():
			state.Transition(StateCancelled, TransitionAborted, ctx.Err().Error())
			return &LoopResult{ExitReason: ExitAborted, State: state, Error: ctx.Err()}
		default:
		}

		// 2. Proactive context compression (Phase 3).
		if config.ContextManager != nil {
			result := config.ContextManager.PreRequest(ctx, state.Messages)
			if result.Compacted {
				state.Messages = result.Messages
				state.HasAttemptedCompact = true
				state.Transition(StateAnalyzing, TransitionAutoCompact,
					fmt.Sprintf("%s: %d→%d tokens", result.Strategy, result.TokensBefore, result.TokensAfter))
				emitEvent(events, EventCompactCompleted, state, result.Strategy)
			}
		}

		// 3. Build API request.
		req := buildRequest(config, state)
		emitEvent(events, EventAPICallStarted, state, "")

		// 4. Call LLM API (streaming or non-streaming).
		var (
			resp              *types.ChatCompletionResponse
			streamToolResults []types.Message // populated only in streaming mode
			err               error
		)

		var streamMetrics *StreamMetrics
		if config.EnableStreaming {
			resp, streamToolResults, streamMetrics, err = callAPIStreaming(ctx, client, config, state, events, logger)
			if streamMetrics != nil {
				state.StreamMetrics = streamMetrics
			}
		} else {
			resp, err = client.ChatCompletion(ctx, req)
		}

		if err != nil {
			if isPromptTooLongError(err) {
				// Phase 3: try reactive compact before giving up.
				if config.ContextManager != nil && !state.HasAttemptedCompact {
					result := config.ContextManager.ReactiveCompact(ctx, state.Messages)
					if result.Compacted && result.Error == nil {
						state.Messages = result.Messages
						state.HasAttemptedCompact = true
						state.Transition(StateAnalyzing, TransitionReactiveCompact,
							fmt.Sprintf("%d→%d tokens", result.TokensBefore, result.TokensAfter))
						emitEvent(events, EventCompactCompleted, state, "reactive")
						continue // retry this turn
					}
				}
				state.Transition(StateError, TransitionPromptTooLong, err.Error())
				return &LoopResult{ExitReason: ExitPromptTooLong, State: state, Error: err}
			}
			state.Transition(StateError, TransitionModelError, err.Error())
			return &LoopResult{ExitReason: ExitModelError, State: state, Error: err}
		}
		_ = streamToolResults // used below in tool_calls branch

		// Phase 3: Token budget check.
		if config.TokenBudget != nil && resp != nil && resp.Usage != nil {
			config.TokenBudget.RecordUsage(*resp.Usage)
			if reason := config.TokenBudget.Check(); reason != "" {
				result := fallbackManualReview(state, fmt.Errorf("token budget: %s", string(reason)))
				state.Transition(StateDecided, TransitionBudgetExhausted, string(reason))
				emitEvent(events, EventBudgetWarning, state, string(reason))
				return &LoopResult{ExitReason: ExitCompleted, ReviewResult: result, State: state}
			}
		}

		if resp == nil || len(resp.Choices) == 0 {
			state.Transition(StateError, TransitionModelError, "empty choices")
			return &LoopResult{
				ExitReason: ExitModelError, State: state,
				Error: fmt.Errorf("LLM returned empty choices"),
			}
		}

		choice := resp.Choices[0]
		assistantMsg := choice.Message

		// 4. Append assistant message (append-only).
		state.AppendMessage(assistantMsg)

		// 5. Branch on finish_reason.
		switch choice.FinishReason {

		case "stop":
			return handleStop(state, config, assistantMsg, events, logger)

		case "tool_calls":
			if len(streamToolResults) > 0 {
				// Streaming mode: tools already executed during stream.
				// Build ToolCallID→ToolCall lookup for reliable matching
				// (index alignment between streamToolResults and ToolCalls not guaranteed).
				tcByID := make(map[string]types.ToolCall, len(assistantMsg.ToolCalls))
				for _, tc := range assistantMsg.ToolCalls {
					tcByID[tc.ID] = tc
				}
				for _, tr := range streamToolResults {
					if tc, ok := tcByID[tr.ToolCallID]; ok {
						emitEvent(events, EventToolCallStarted, state, tc.Function.Name)
						state.AppendTrace(fmt.Sprintf("tool_call:%s", tc.Function.Name))
						if len(config.PostToolHooks) > 0 {
							runPostToolHooks(config.PostToolHooks, tc.Function.Name, tr.Content.String(), nil, logger)
						}
						emitEvent(events, EventToolCallCompleted, state, tc.Function.Name)
						state.AppendTrace(fmt.Sprintf("tool_result:%s", tc.Function.Name))
					}
					state.AppendMessage(tr)
				}
				state.TurnCount++
				state.Transition(StateAnalyzing, TransitionNextTurn,
					fmt.Sprintf("turn %d, %d tools (streamed)", state.TurnCount, len(streamToolResults)))
				emitEvent(events, EventTurnStarted, state, "")
			} else if err := handleToolCalls(ctx, state, config, assistantMsg, events, logger); err != nil {
				// Non-streaming: original path.
				state.Transition(StateError, TransitionModelError, err.Error())
				return &LoopResult{ExitReason: ExitModelError, State: state, Error: err}
			}

		case "length":
			if !handleMaxOutputTokens(state, config, logger) {
				// Recovery exhausted → fallback.
				logger.Warn("max_output_tokens recovery exhausted, falling back to MANUAL_REVIEW")
				result := fallbackManualReview(state,
					fmt.Errorf("output truncated, recovery exhausted after %d attempts", state.MaxOutputRecoveryCount))
				state.Transition(StateDecided, TransitionCompleted, "recovery exhausted, fallback")
				emitEvent(events, EventTurnCompleted, state, "")
				return &LoopResult{ExitReason: ExitCompleted, ReviewResult: result, State: state}
			}
			emitEvent(events, EventRecoveryAttempt, state, "max_output_tokens")

		default:
			logger.Warn("unexpected finish_reason", slog.String("finish_reason", choice.FinishReason))
		}
	}

	// Loop exit: maxTurns exceeded.
	result := fallbackManualReview(state, fmt.Errorf("max turns (%d) exceeded", config.MaxTurns))
	state.Transition(StateDecided, TransitionMaxTurns,
		fmt.Sprintf("reached max turns %d", config.MaxTurns))
	emitEvent(events, EventTurnCompleted, state, "")
	return &LoopResult{ExitReason: ExitMaxTurns, ReviewResult: result, State: state}
}

// handleStop processes a "stop" finish_reason: parse the LLM output into ReviewResult.
func handleStop(state *State, config *LoopConfig, msg types.Message, events chan<- StreamEvent, logger *slog.Logger) *LoopResult {
	state.Transition(StateJudging, TransitionCompleted, "LLM produced final output")
	state.AppendTrace("LLM final judgment")

	result, err := parseReviewResult(msg.Content.String(), state, config)
	if err != nil {
		logger.Warn("failed to parse review result, forcing MANUAL_REVIEW",
			slog.String("error", err.Error()),
			slog.String("raw", truncate(msg.Content.String(), 200)),
		)
		result = fallbackManualReview(state, err)
	}

	state.Transition(StateDecided, TransitionCompleted,
		fmt.Sprintf("decision=%s confidence=%.2f", result.Decision, result.Confidence))
	emitEvent(events, EventTurnCompleted, state, string(result.Decision))
	return &LoopResult{ExitReason: ExitCompleted, ReviewResult: result, State: state}
}

// handleToolCalls processes tool_calls: execute tools → append results → continue loop.
func handleToolCalls(ctx context.Context, state *State, config *LoopConfig, msg types.Message, events chan<- StreamEvent, logger *slog.Logger) error {
	if len(msg.ToolCalls) == 0 {
		logger.Warn("finish_reason=tool_calls but no tool_calls in message")
		return nil
	}

	// Phase 5: PreToolHook per-tool check — blocked tools get error messages.
	var allowedCalls []types.ToolCall
	var blockedResults []types.Message
	for _, tc := range msg.ToolCalls {
		if len(config.PreToolHooks) > 0 {
			if err := runPreToolHooks(config.PreToolHooks, tc.Function.Name, tc.Function.Arguments, logger); err != nil {
				blockedResults = append(blockedResults, types.Message{
					Role:       types.RoleTool,
					Content:    types.NewTextContent(fmt.Sprintf(`{"error":"blocked by hook: %s"}`, err.Error())),
					ToolCallID: tc.ID,
				})
				state.AppendTrace(fmt.Sprintf("tool_blocked:%s", tc.Function.Name))
				continue
			}
		}
		allowedCalls = append(allowedCalls, tc)
		emitEvent(events, EventToolCallStarted, state, tc.Function.Name)
		state.AppendTrace(fmt.Sprintf("tool_call:%s", tc.Function.Name))
	}

	// Execute non-blocked tools.
	var toolResults []types.Message
	if len(allowedCalls) > 0 {
		var err error
		toolResults, err = config.ToolExecutor.Execute(ctx, allowedCalls)
		if err != nil {
			logger.Error("tool execution failed", slog.String("error", err.Error()))
			toolResults = buildToolErrorMessages(allowedCalls, err)
		}
	}

	// Phase 5: PostToolHook per-tool after execution.
	if len(config.PostToolHooks) > 0 {
		for i, tc := range allowedCalls {
			result := ""
			if i < len(toolResults) {
				result = toolResults[i].Content.String()
			}
			runPostToolHooks(config.PostToolHooks, tc.Function.Name, result, nil, logger)
		}
	}

	// Merge blocked + executed results, append to state.
	allResults := append(blockedResults, toolResults...)
	for _, tr := range allResults {
		state.AppendMessage(tr)
	}

	for _, tc := range allowedCalls {
		emitEvent(events, EventToolCallCompleted, state, tc.Function.Name)
		state.AppendTrace(fmt.Sprintf("tool_result:%s", tc.Function.Name))
	}

	state.TurnCount++
	state.Transition(StateAnalyzing, TransitionNextTurn,
		fmt.Sprintf("turn %d, %d tools executed", state.TurnCount, len(allowedCalls)))
	emitEvent(events, EventTurnStarted, state, "")
	return nil
}

// parseReviewResult extracts a ReviewResult from LLM output text.
// Handles multiple formats: raw JSON, ```json fenced, or JSON embedded in text.
// Applies confidence threshold and AllowAutoReject enforcement.
func parseReviewResult(content string, state *State, config *LoopConfig) (*types.ReviewResult, error) {
	jsonStr := extractJSON(content)
	if jsonStr == "" {
		return nil, fmt.Errorf("no JSON found in LLM output")
	}

	var raw struct {
		Decision   string          `json:"decision"`
		Confidence float64         `json:"confidence"`
		Violations json.RawMessage `json:"violations"`
		Reasoning  string          `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, fmt.Errorf("parsing review JSON: %w", err)
	}

	// Parse violations: handle both []PolicyViolation and []string formats.
	// LLMs (especially Adjudicator) sometimes simplify violations to string arrays.
	violations := parseViolations(raw.Violations)

	decision := types.ReviewDecision(raw.Decision)

	// Validate decision value.
	switch decision {
	case types.DecisionPassed, types.DecisionRejected, types.DecisionManualReview:
	default:
		return nil, fmt.Errorf("invalid decision value: %q", raw.Decision)
	}

	// Enforce AllowAutoReject: when disabled, REJECTED → MANUAL_REVIEW.
	if decision == types.DecisionRejected && !config.AllowAutoReject {
		decision = types.DecisionManualReview
		state.AppendTrace("auto_reject_disabled: downgraded REJECTED → MANUAL_REVIEW")
	}

	// Enforce confidence threshold: low confidence REJECTED → MANUAL_REVIEW.
	if decision == types.DecisionRejected && raw.Confidence < config.ConfidenceThreshold {
		decision = types.DecisionManualReview
		state.AppendTrace(fmt.Sprintf("confidence %.2f < threshold %.2f: downgraded REJECTED → MANUAL_REVIEW",
			raw.Confidence, config.ConfidenceThreshold))
	}

	riskLevel := types.RiskMedium
	for _, v := range violations {
		if v.Severity == "critical" {
			riskLevel = types.RiskCritical
			break
		}
		if v.Severity == "high" {
			riskLevel = types.RiskHigh
		}
	}

	return &types.ReviewResult{
		AdID:           state.AdContent.ID,
		Decision:       decision,
		Confidence:     raw.Confidence,
		Violations:     violations,
		RiskLevel:      riskLevel,
		AgentTrace:     state.PartialResult.AgentTrace,
		ReviewDuration: time.Since(state.StartedAt),
		Timestamp:      time.Now(),
	}, nil
}

// fallbackManualReview creates a safe MANUAL_REVIEW result when the loop cannot
// produce a definitive decision. This is the fail-closed principle in action.
func fallbackManualReview(state *State, reason error) *types.ReviewResult {
	return &types.ReviewResult{
		AdID:           state.AdContent.ID,
		Decision:       types.DecisionManualReview,
		Confidence:     0.0,
		Violations:     state.PartialResult.Violations,
		RiskLevel:      types.RiskMedium,
		AgentTrace:     append(state.PartialResult.AgentTrace, fmt.Sprintf("fallback: %v", reason)),
		ReviewDuration: time.Since(state.StartedAt),
		Timestamp:      time.Now(),
	}
}

// --- Helper functions ---

// buildRequest constructs the ChatCompletionRequest for the current state.
func buildRequest(config *LoopConfig, state *State) types.ChatCompletionRequest {
	maxTokens := config.DefaultMaxTokens
	if state.MaxTokensOverride != nil {
		maxTokens = *state.MaxTokensOverride
	}
	req := types.ChatCompletionRequest{
		Messages:  state.Messages,
		Tools:     config.Tools,
		MaxTokens: &maxTokens,
	}
	if config.Model != "" {
		req.Model = config.Model
		req.RetryCurrentModel = config.Model
		req.RetryFallbackModel = config.FallbackModel
	}
	return req
}

// buildToolErrorMessages creates error tool_result messages for failed tool calls.
// This keeps the loop running (fail-closed) instead of crashing.
func buildToolErrorMessages(toolCalls []types.ToolCall, err error) []types.Message {
	results := make([]types.Message, 0, len(toolCalls))
	for _, tc := range toolCalls {
		results = append(results, types.Message{
			Role:       types.RoleTool,
			Content:    types.NewTextContent(fmt.Sprintf(`{"error":"%s"}`, err.Error())),
			ToolCallID: tc.ID,
		})
	}
	return results
}

// extractJSON extracts a JSON object from LLM output text.
// Tries: raw JSON → ```json fenced → first {...} substring.
func extractJSON(content string) string {
	content = strings.TrimSpace(content)

	if json.Valid([]byte(content)) {
		return content
	}

	// Try ```json ... ``` block.
	if idx := strings.Index(content, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(content[start:], "```"); end >= 0 {
			candidate := strings.TrimSpace(content[start : start+end])
			if json.Valid([]byte(candidate)) {
				return candidate
			}
		}
	}

	// Try first { ... } block.
	if start := strings.Index(content, "{"); start >= 0 {
		if end := strings.LastIndex(content, "}"); end > start {
			candidate := content[start : end+1]
			if json.Valid([]byte(candidate)) {
				return candidate
			}
		}
	}
	return ""
}

// isPromptTooLongError checks if the error is a prompt-too-long error.
func isPromptTooLongError(err error) bool {
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 400 &&
			strings.Contains(strings.ToLower(apiErr.Message), "too long")
	}
	return false
}

// emitEvent sends an event to the channel. Nil-safe and non-blocking.
func emitEvent(events chan<- StreamEvent, eventType EventType, state *State, detail string) {
	if events == nil {
		return
	}
	select {
	case events <- StreamEvent{
		Type:      eventType,
		State:     state.LoopState,
		TurnCount: state.TurnCount,
		Timestamp: time.Now(),
		Detail:    detail,
	}:
	default:
	}
}

// parseViolations handles both []PolicyViolation and []string formats.
// LLMs sometimes output violations as string arrays ["POL_001","POL_002"]
// instead of full objects. This function normalizes both to []PolicyViolation.
func parseViolations(raw json.RawMessage) []types.PolicyViolation {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	// Try full PolicyViolation objects first.
	var full []types.PolicyViolation
	if err := json.Unmarshal(raw, &full); err == nil {
		return full
	}

	// Fallback: try string array (policy IDs only).
	var ids []string
	if err := json.Unmarshal(raw, &ids); err == nil {
		violations := make([]types.PolicyViolation, len(ids))
		for i, id := range ids {
			violations[i] = types.PolicyViolation{PolicyID: id, Severity: "unknown", Description: id}
		}
		return violations
	}

	return nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// callAPIStreaming uses the streaming API path: SSE chunks are accumulated
// by StreamAccumulator, and tool calls are dispatched to StreamingToolExecutor
// as soon as their parameters are complete (not waiting for the full response).
//
// On streaming failure, falls back to non-streaming (watchdog fallback pattern).
// Returns the assembled response + any tool results executed during streaming.
func callAPIStreaming(
	ctx context.Context,
	client llm.LLMClient,
	config *LoopConfig,
	state *State,
	events chan<- StreamEvent,
	logger *slog.Logger,
) (*types.ChatCompletionResponse, []types.Message, *StreamMetrics, error) {
	req := buildRequest(config, state)
	req.Stream = true

	streamStart := time.Now()
	stream, err := client.StreamChatCompletion(ctx, req)
	if err != nil || stream == nil {
		if err != nil {
			logger.Warn("streaming connection failed, falling back to non-streaming",
				slog.String("error", err.Error()))
		} else {
			logger.Warn("streaming returned nil reader, falling back to non-streaming")
		}
		emitEvent(events, EventStreamFallback, state, "connection failed")
		req.Stream = false
		resp, err := client.ChatCompletion(ctx, req)
		return resp, nil, nil, err
	}
	defer stream.Close()
	emitEvent(events, EventStreamStarted, state, "")

	executor := NewStreamingToolExecutor(ctx, config.ToolExecutor, nil, config.PreToolHooks, config.PostToolHooks, logger)
	accumulator := NewStreamAccumulator(executor, logger)

	prevToolCount := 0
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			logger.Warn("stream interrupted, falling back to non-streaming",
				slog.String("error", err.Error()))
			emitEvent(events, EventStreamFallback, state, "stream interrupted")
			req.Stream = false
			resp, err := client.ChatCompletion(ctx, req)
			return resp, nil, nil, err
		}
		accumulator.ProcessChunk(chunk)

		// Emit events for tools dispatched during streaming.
		if newCount := executor.ToolCount(); newCount > prevToolCount {
			for i := prevToolCount; i < newCount; i++ {
				emitEvent(events, EventStreamToolDispatched, state,
					fmt.Sprintf("tool dispatched during stream (%s elapsed)",
						time.Since(streamStart).Round(time.Millisecond)))
			}
			prevToolCount = newCount
		}
	}
	accumulator.Finalize()

	// Check for tools dispatched by Finalize (last tool call in stream).
	if newCount := executor.ToolCount(); newCount > prevToolCount {
		for i := prevToolCount; i < newCount; i++ {
			emitEvent(events, EventStreamToolDispatched, state,
				fmt.Sprintf("tool dispatched at stream end (%s elapsed)",
					time.Since(streamStart).Round(time.Millisecond)))
		}
	}

	streamDuration := time.Since(streamStart)

	// Collect results from tools that executed during streaming.
	var (
		toolResults []types.Message
		collectWait time.Duration
	)
	if executor.ToolCount() > 0 {
		collectStart := time.Now()
		toolResults = executor.CollectResults(ctx)
		collectWait = time.Since(collectStart)

		logger.Info("streaming tool execution summary",
			slog.Int("tools", executor.ToolCount()),
			slog.Duration("stream_duration", streamDuration),
			slog.Duration("collect_wait", collectWait),
		)

		for _, tr := range toolResults {
			emitEvent(events, EventStreamToolCompleted, state, tr.ToolCallID)
		}
	}

	metrics := &StreamMetrics{
		StreamDuration:  streamDuration,
		CollectWait:     collectWait,
		ToolsDispatched: executor.ToolCount(),
	}

	resp := accumulator.BuildResponse()
	return resp, toolResults, metrics, nil
}
