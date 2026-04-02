package compact

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/types"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- Token estimation tests ---

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"", 0},
		{"hi", 1},
		{"hello world", 4},                       // 11 chars / 3 + 1 = 4
		{strings.Repeat("a", 300), 101},           // 300/3 + 1 = 101
	}
	for _, tt := range tests {
		got := EstimateTokens(tt.input)
		if got != tt.expected {
			t.Errorf("EstimateTokens(%q): expected %d, got %d", tt.input[:min(len(tt.input), 20)], tt.expected, got)
		}
	}
}

func TestEstimateMessagesTokens(t *testing.T) {
	msgs := []types.Message{
		{Role: types.RoleSystem, Content: types.NewTextContent("You are an agent")},
		{Role: types.RoleUser, Content: types.NewTextContent("Review this ad")},
	}
	tokens := EstimateMessagesTokens(msgs)
	// Each message: content tokens + 4 overhead.
	// "You are an agent" = 16 chars / 3 + 1 = 6 tokens + 4 = 10
	// "Review this ad" = 14 chars / 3 + 1 = 5 tokens + 4 = 9
	// Total = 19
	if tokens < 15 || tokens > 25 {
		t.Errorf("expected ~19 tokens, got %d", tokens)
	}
}

// --- Prompt tests ---

func TestFormatCompactSummary_ExtractsSummary(t *testing.T) {
	raw := `<analysis>
Some analysis here that should be discarded.
</analysis>

<summary>
1. Reviewed ads: ad_001 PASSED, ad_002 REJECTED
2. Violations: POL_001 triggered twice
</summary>`

	summary := FormatCompactSummary(raw)
	if !strings.Contains(summary, "ad_001 PASSED") {
		t.Errorf("expected summary content, got: %s", summary)
	}
	if strings.Contains(summary, "analysis") {
		t.Errorf("analysis section should be discarded, got: %s", summary)
	}
}

func TestFormatCompactSummary_NoTags(t *testing.T) {
	raw := "Plain text summary without tags"
	summary := FormatCompactSummary(raw)
	if summary != raw {
		t.Errorf("expected raw passthrough, got: %s", summary)
	}
}

func TestBuildCompactPrompt_ContainsKey(t *testing.T) {
	prompt := BuildCompactPrompt()
	for _, key := range []string{
		"CRITICAL",
		"Do NOT call any tools",
		"<analysis>",
		"<summary>",
		"Reviewed Ads and Decisions",
		"Violation Patterns",
		"False Positive",
		"Regional Compliance",
		"Advertiser Reputation",
		"Pending Tasks",
	} {
		if !strings.Contains(prompt, key) {
			t.Errorf("prompt missing key phrase: %q", key)
		}
	}
}

// --- MicroCompact tests ---

func buildTestMessages(toolCount int) []types.Message {
	msgs := []types.Message{
		{Role: types.RoleSystem, Content: types.NewTextContent("system prompt")},
		{Role: types.RoleUser, Content: types.NewTextContent("review this ad")},
	}
	for i := 0; i < toolCount; i++ {
		// Assistant with tool call.
		msgs = append(msgs, types.Message{
			Role:    types.RoleAssistant,
			Content: types.NewTextContent(""),
			ToolCalls: []types.ToolCall{{
				ID:       fmt.Sprintf("call_%d", i),
				Type:     "function",
				Function: types.ToolCallFunction{Name: "analyze_content"},
			}},
		})
		// Tool result.
		msgs = append(msgs, types.Message{
			Role:       types.RoleTool,
			Content:    types.NewTextContent(fmt.Sprintf(`{"signals":[],"tool_index":%d}`, i)),
			ToolCallID: fmt.Sprintf("call_%d", i),
		})
	}
	return msgs
}

func TestMicroCompact_ClearsOldToolResults(t *testing.T) {
	cm := NewContextManager(CompactConfig{MicroCompactKeepRecent: 2}, nil, testLogger())

	// 5 tool results: should clear first 3, keep last 2.
	msgs := buildTestMessages(5)
	result := cm.microCompact(msgs)

	toolResults := 0
	cleared := 0
	for _, msg := range result {
		if msg.Role == types.RoleTool {
			toolResults++
			if msg.Content.String() == microCompactPlaceholder {
				cleared++
			}
		}
	}
	if toolResults != 5 {
		t.Errorf("expected 5 tool results, got %d", toolResults)
	}
	if cleared != 3 {
		t.Errorf("expected 3 cleared, got %d", cleared)
	}
}

func TestMicroCompact_PreservesNonToolMessages(t *testing.T) {
	cm := NewContextManager(CompactConfig{MicroCompactKeepRecent: 1}, nil, testLogger())

	msgs := buildTestMessages(3)
	result := cm.microCompact(msgs)

	// System and user messages should be unchanged.
	if result[0].Content.String() != "system prompt" {
		t.Errorf("system message changed: %s", result[0].Content.String())
	}
	if result[1].Content.String() != "review this ad" {
		t.Errorf("user message changed: %s", result[1].Content.String())
	}
}

func TestMicroCompact_FewerThanKeep(t *testing.T) {
	cm := NewContextManager(CompactConfig{MicroCompactKeepRecent: 10}, nil, testLogger())

	msgs := buildTestMessages(3)
	result := cm.microCompact(msgs)

	// All tool results should be intact (3 < 10).
	for _, msg := range result {
		if msg.Role == types.RoleTool && msg.Content.String() == microCompactPlaceholder {
			t.Error("no tool results should be cleared when below keep limit")
		}
	}
}

// --- AutoCompact tests ---

// mockCompactLLM is a minimal LLM client for compact tests.
type mockCompactLLM struct {
	response *types.ChatCompletionResponse
	err      error
	calls    int
}

func (m *mockCompactLLM) ChatCompletion(_ context.Context, _ types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	m.calls++
	return m.response, m.err
}

func (m *mockCompactLLM) StreamChatCompletion(_ context.Context, _ types.ChatCompletionRequest) (*llm.StreamReader, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockCompactLLM) Usage() *llm.SessionUsage { return llm.NewSessionUsage() }

func TestAutoCompact_TriggersAboveThreshold(t *testing.T) {
	mockLLM := &mockCompactLLM{
		response: &types.ChatCompletionResponse{
			Choices: []types.Choice{{
				Message: types.Message{
					Role: types.RoleAssistant,
					Content: types.NewTextContent(`<analysis>analysis</analysis>
<summary>Summary of 3 reviewed ads: ad_001 PASSED, ad_002 REJECTED, ad_003 MANUAL_REVIEW. Main violations: POL_001 medical claims.</summary>`),
				},
				FinishReason: "stop",
			}},
		},
	}

	cfg := CompactConfig{
		ContextWindowSize:      1000,
		AutoCompactBuffer:      100,
		SummaryOutputReserve:   100,
		MicroCompactKeepRecent: 2,
		MaxConsecutiveFailures: 3,
	}
	// Threshold = 1000 - 100 - 100 = 800 tokens.
	cm := NewContextManager(cfg, mockLLM, testLogger())

	// Build messages that exceed 800 tokens.
	msgs := []types.Message{
		{Role: types.RoleSystem, Content: types.NewTextContent(strings.Repeat("x", 1500))},
		{Role: types.RoleUser, Content: types.NewTextContent(strings.Repeat("y", 1500))},
	}

	result := cm.PreRequest(context.Background(), msgs)
	if !result.Compacted {
		t.Error("expected compaction to trigger")
	}
	if result.Strategy != "auto" {
		t.Errorf("expected auto strategy, got %s", result.Strategy)
	}
	if mockLLM.calls != 1 {
		t.Errorf("expected 1 LLM call, got %d", mockLLM.calls)
	}
	if result.TokensAfter >= result.TokensBefore {
		t.Errorf("tokens should decrease: %d → %d", result.TokensBefore, result.TokensAfter)
	}
}

func TestAutoCompact_SkipsBelowThreshold(t *testing.T) {
	mockLLM := &mockCompactLLM{}
	cfg := CompactConfig{
		ContextWindowSize:      100000,
		AutoCompactBuffer:      13000,
		SummaryOutputReserve:   8000,
		MicroCompactKeepRecent: 6,
		MaxConsecutiveFailures: 3,
	}
	cm := NewContextManager(cfg, mockLLM, testLogger())

	// Small messages — well below threshold.
	msgs := []types.Message{
		{Role: types.RoleSystem, Content: types.NewTextContent("short")},
		{Role: types.RoleUser, Content: types.NewTextContent("short")},
	}

	result := cm.PreRequest(context.Background(), msgs)
	if result.Strategy == "auto" {
		t.Error("auto compact should not trigger below threshold")
	}
	if mockLLM.calls != 0 {
		t.Errorf("no LLM calls expected, got %d", mockLLM.calls)
	}
}

func TestAutoCompact_CircuitBreaker(t *testing.T) {
	mockLLM := &mockCompactLLM{err: fmt.Errorf("LLM error")}
	cfg := CompactConfig{
		ContextWindowSize:      100,
		AutoCompactBuffer:      10,
		SummaryOutputReserve:   10,
		MicroCompactKeepRecent: 2,
		MaxConsecutiveFailures: 3,
	}
	cm := NewContextManager(cfg, mockLLM, testLogger())

	msgs := []types.Message{
		{Role: types.RoleSystem, Content: types.NewTextContent(strings.Repeat("x", 500))},
		{Role: types.RoleUser, Content: types.NewTextContent(strings.Repeat("y", 500))},
	}

	// Three failures should trip the circuit breaker.
	for i := 0; i < 3; i++ {
		cm.PreRequest(context.Background(), msgs)
	}

	if cm.state.ConsecutiveFailures != 3 {
		t.Errorf("expected 3 failures, got %d", cm.state.ConsecutiveFailures)
	}

	// Fourth attempt should skip without LLM call.
	callsBefore := mockLLM.calls
	cm.PreRequest(context.Background(), msgs)
	if mockLLM.calls != callsBefore {
		t.Error("circuit breaker should prevent additional LLM calls")
	}
}

func TestReactiveCompact_Succeeds(t *testing.T) {
	mockLLM := &mockCompactLLM{
		response: &types.ChatCompletionResponse{
			Choices: []types.Choice{{
				Message: types.Message{
					Role: types.RoleAssistant,
					Content: types.NewTextContent(`<summary>Reactive summary of all previous reviews and findings for continuity.</summary>`),
				},
				FinishReason: "stop",
			}},
		},
	}
	cfg := DefaultCompactConfig()
	cm := NewContextManager(cfg, mockLLM, testLogger())

	msgs := []types.Message{
		{Role: types.RoleSystem, Content: types.NewTextContent("system")},
		{Role: types.RoleUser, Content: types.NewTextContent("review")},
	}

	result := cm.ReactiveCompact(context.Background(), msgs)
	if !result.Compacted {
		t.Error("expected reactive compact to succeed")
	}
	if result.Strategy != "reactive" {
		t.Errorf("expected reactive strategy, got %s", result.Strategy)
	}
}

func TestPreRequest_MicroThenAuto(t *testing.T) {
	mockLLM := &mockCompactLLM{
		response: &types.ChatCompletionResponse{
			Choices: []types.Choice{{
				Message: types.Message{
					Role:    types.RoleAssistant,
					Content: types.NewTextContent(`<summary>Combined micro and auto compact summary with review decisions preserved.</summary>`),
				},
				FinishReason: "stop",
			}},
		},
	}

	cfg := CompactConfig{
		ContextWindowSize:      200,
		AutoCompactBuffer:      20,
		SummaryOutputReserve:   20,
		MicroCompactKeepRecent: 1,
		MaxConsecutiveFailures: 3,
	}
	cm := NewContextManager(cfg, mockLLM, testLogger())

	// Build messages with many tool results that exceed threshold even after micro.
	msgs := buildTestMessages(10)
	// Pad system prompt to ensure over threshold.
	msgs[0] = types.Message{Role: types.RoleSystem, Content: types.NewTextContent(strings.Repeat("s", 500))}

	result := cm.PreRequest(context.Background(), msgs)
	if !result.Compacted {
		t.Error("expected compaction")
	}
	// Should be "auto" since micro alone won't get below threshold.
	if result.Strategy != "auto" {
		t.Errorf("expected auto strategy after micro, got %s", result.Strategy)
	}
}
