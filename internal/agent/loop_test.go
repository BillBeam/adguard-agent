package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/agent/mock"
	"github.com/BillBeam/adguard-agent/internal/compact"
	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/types"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func testAd() *types.AdContent {
	return &types.AdContent{
		ID:           "test_ad_001",
		Type:         "text",
		Region:       "US",
		Category:     "healthcare",
		AdvertiserID: "adv_test",
		Content: types.AdBody{
			Headline: "Miracle Cure for Diabetes",
			Body:     "100% effective treatment, FDA approved",
			CTA:      "Buy Now",
		},
		LandingPage: types.LandingPage{
			URL:               "https://example.com/cure",
			Description:       "Product page with fake FDA logo",
			IsAccessible:      true,
			IsMobileOptimized: true,
		},
	}
}

func testConfig() *LoopConfig {
	return &LoopConfig{
		MaxTurns:            6,
		ConfidenceThreshold: 0.70,
		AllowAutoReject:     true,
		RequireVerification: false,
		Pipeline:            "standard",
		Tools:               mock.ToolDefinitions(),
		ToolExecutor:        mock.NewToolExecutor(),
		SystemPrompt:        "You are a test review agent.",
		DefaultMaxTokens:    DefaultMaxOutputTokens,
		EscalatedMaxTokens:  EscalatedMaxOutputTokens,
		MaxRecoveryAttempts: MaxRecoveryAttempts,
	}
}

// makeStopResponse creates a mock response where LLM outputs a final review decision.
func makeStopResponse(decision string, confidence float64, violations []types.PolicyViolation) *types.ChatCompletionResponse {
	v, _ := json.Marshal(violations)
	content := fmt.Sprintf(`{"decision":"%s","confidence":%.2f,"violations":%s,"reasoning":"test"}`,
		decision, confidence, string(v))
	return &types.ChatCompletionResponse{
		Choices: []types.Choice{{
			Message:      types.Message{Role: types.RoleAssistant, Content: types.NewTextContent(content)},
			FinishReason: "stop",
		}},
	}
}

// makeToolCallResponse creates a mock response where LLM requests tool calls.
func makeToolCallResponse(toolNames ...string) *types.ChatCompletionResponse {
	calls := make([]types.ToolCall, len(toolNames))
	for i, name := range toolNames {
		args := `{"headline":"test","body":"test"}`
		if name == "match_policies" {
			args = `{"region":"US","category":"healthcare","signals":["unverified_medical_claim"]}`
		}
		if name == "check_landing_page" {
			args = `{"url":"https://example.com","description":"test page","is_accessible":true}`
		}
		calls[i] = types.ToolCall{
			ID:       fmt.Sprintf("call_%s_%d", name, i),
			Type:     "function",
			Function: types.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)},
		}
	}
	return &types.ChatCompletionResponse{
		Choices: []types.Choice{{
			Message: types.Message{
				Role:      types.RoleAssistant,
				Content:   types.NewTextContent(""),
				ToolCalls: calls,
			},
			FinishReason: "tool_calls",
		}},
	}
}

// makeLengthResponse creates a mock response with finish_reason="length" (truncated output).
func makeLengthResponse() *types.ChatCompletionResponse {
	return &types.ChatCompletionResponse{
		Choices: []types.Choice{{
			Message:      types.Message{Role: types.RoleAssistant, Content: types.NewTextContent("partial output...")},
			FinishReason: "length",
		}},
	}
}

// --- Tests ---

func TestRun_NormalCompletion_Rejected(t *testing.T) {
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeToolCallResponse("analyze_content"),
		makeToolCallResponse("match_policies"),
		makeStopResponse("REJECTED", 0.95, []types.PolicyViolation{{
			PolicyID: "POL_001", Severity: "critical", Description: "Unverified medical claim",
			Confidence: 0.95, Evidence: "miracle cure",
		}}),
	}

	state := NewState(testAd())
	config := testConfig()
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), client, config, state, nil, testLogger())

	if result.ExitReason != ExitCompleted {
		t.Errorf("ExitReason = %s, want completed", result.ExitReason)
	}
	if result.ReviewResult == nil {
		t.Fatal("ReviewResult is nil")
	}
	if result.ReviewResult.Decision != types.DecisionRejected {
		t.Errorf("Decision = %s, want REJECTED", result.ReviewResult.Decision)
	}
	if result.ReviewResult.Confidence != 0.95 {
		t.Errorf("Confidence = %f, want 0.95", result.ReviewResult.Confidence)
	}
	if state.TurnCount != 2 {
		t.Errorf("TurnCount = %d, want 2", state.TurnCount)
	}
	if len(result.ReviewResult.Violations) != 1 {
		t.Errorf("Violations = %d, want 1", len(result.ReviewResult.Violations))
	}
}

func TestRun_NormalCompletion_Passed(t *testing.T) {
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeStopResponse("PASSED", 0.88, nil),
	}

	state := NewState(testAd())
	config := testConfig()
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), client, config, state, nil, testLogger())

	if result.ExitReason != ExitCompleted {
		t.Errorf("ExitReason = %s, want completed", result.ExitReason)
	}
	if result.ReviewResult.Decision != types.DecisionPassed {
		t.Errorf("Decision = %s, want PASSED", result.ReviewResult.Decision)
	}
	if state.TurnCount != 0 {
		t.Errorf("TurnCount = %d, want 0", state.TurnCount)
	}
}

func TestRun_MaxTurns(t *testing.T) {
	client := mock.NewLLMClient()
	// All responses are tool_calls — loop will hit maxTurns.
	for i := 0; i < 10; i++ {
		client.Responses = append(client.Responses, makeToolCallResponse("analyze_content"))
	}

	state := NewState(testAd())
	config := testConfig()
	config.MaxTurns = 3
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), client, config, state, nil, testLogger())

	if result.ExitReason != ExitMaxTurns {
		t.Errorf("ExitReason = %s, want max_turns", result.ExitReason)
	}
	if result.ReviewResult.Decision != types.DecisionManualReview {
		t.Errorf("Decision = %s, want MANUAL_REVIEW (fallback)", result.ReviewResult.Decision)
	}
	if state.TurnCount != 3 {
		t.Errorf("TurnCount = %d, want 3", state.TurnCount)
	}
}

func TestRun_ContextCancellation(t *testing.T) {
	// Pre-cancel the context so the loop detects cancellation on the second iteration.
	ctx, cancel := context.WithCancel(context.Background())

	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeToolCallResponse("analyze_content"),
		// Second call won't happen — context will be cancelled.
		makeStopResponse("PASSED", 0.9, nil),
	}

	state := NewState(testAd())
	config := testConfig()
	state.Messages = buildInitialMessages(config.SystemPrompt)

	// Cancel right after first tool call but before next loop iteration.
	origExecutor := config.ToolExecutor
	config.ToolExecutor = &cancellingExecutor{inner: origExecutor, cancel: cancel}

	result := Run(ctx, client, config, state, nil, testLogger())

	if result.ExitReason != ExitAborted {
		t.Errorf("ExitReason = %s, want aborted", result.ExitReason)
	}
	if state.LoopState != StateCancelled {
		t.Errorf("LoopState = %s, want CANCELLED", state.LoopState)
	}
}

// cancellingExecutor wraps a ToolExecutor and cancels the context after execution.
type cancellingExecutor struct {
	inner  ToolExecutor
	cancel context.CancelFunc
}

func (e *cancellingExecutor) Execute(ctx context.Context, toolCalls []types.ToolCall) ([]types.Message, error) {
	results, err := e.inner.Execute(ctx, toolCalls)
	e.cancel() // cancel context after tool execution
	return results, err
}

func TestRun_MaxOutputRecovery_Success(t *testing.T) {
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeLengthResponse(),                            // first: truncated
		makeStopResponse("REJECTED", 0.90, nil), // after escalation: success
	}

	state := NewState(testAd())
	config := testConfig()
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), client, config, state, nil, testLogger())

	if result.ExitReason != ExitCompleted {
		t.Errorf("ExitReason = %s, want completed", result.ExitReason)
	}
	if result.ReviewResult.Decision != types.DecisionRejected {
		t.Errorf("Decision = %s, want REJECTED", result.ReviewResult.Decision)
	}
	// Verify escalation happened.
	if state.MaxTokensOverride == nil {
		t.Error("MaxTokensOverride should be set after escalation")
	}
	// Check transition log contains escalate.
	found := false
	for _, tr := range state.TransitionLog {
		if tr.Reason == TransitionMaxOutputEscalate {
			found = true
			break
		}
	}
	if !found {
		t.Error("TransitionLog should contain max_output_tokens_escalate")
	}
}

func TestRun_MaxOutputRecovery_Exhausted(t *testing.T) {
	client := mock.NewLLMClient()
	// 1 escalate + 3 recovery = 4 length responses, then it gives up.
	// Plus the original truncated content = 5 total length responses.
	for i := 0; i < 6; i++ {
		client.Responses = append(client.Responses, makeLengthResponse())
	}

	state := NewState(testAd())
	config := testConfig()
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), client, config, state, nil, testLogger())

	if result.ExitReason != ExitCompleted {
		t.Errorf("ExitReason = %s, want completed (fallback)", result.ExitReason)
	}
	if result.ReviewResult.Decision != types.DecisionManualReview {
		t.Errorf("Decision = %s, want MANUAL_REVIEW", result.ReviewResult.Decision)
	}
}

func TestRun_ModelError(t *testing.T) {
	client := mock.NewLLMClient()
	client.Errors = []error{
		&llm.APIError{StatusCode: 500, Message: "internal server error"},
	}

	state := NewState(testAd())
	config := testConfig()
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), client, config, state, nil, testLogger())

	if result.ExitReason != ExitModelError {
		t.Errorf("ExitReason = %s, want model_error", result.ExitReason)
	}
	if result.Error == nil {
		t.Error("Error should be set")
	}
}

func TestRun_ToolExecutionError(t *testing.T) {
	// Tool executor that always errors.
	failingExecutor := &failExecutor{}

	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeToolCallResponse("analyze_content"),
		makeStopResponse("MANUAL_REVIEW", 0.50, nil), // LLM handles error gracefully
	}

	state := NewState(testAd())
	config := testConfig()
	config.ToolExecutor = failingExecutor
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), client, config, state, nil, testLogger())

	if result.ExitReason != ExitCompleted {
		t.Errorf("ExitReason = %s, want completed (loop should continue despite tool error)", result.ExitReason)
	}
}

type failExecutor struct{}

func (f *failExecutor) Execute(_ context.Context, _ []types.ToolCall) ([]types.Message, error) {
	return nil, fmt.Errorf("simulated tool infrastructure failure")
}

func TestRun_ConfidenceThreshold(t *testing.T) {
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeStopResponse("REJECTED", 0.50, []types.PolicyViolation{{
			PolicyID: "POL_001", Severity: "critical", Description: "test",
		}}),
	}

	state := NewState(testAd())
	config := testConfig()
	config.ConfidenceThreshold = 0.70
	config.AllowAutoReject = true // even with auto-reject allowed, low confidence → MANUAL_REVIEW
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), client, config, state, nil, testLogger())

	if result.ReviewResult.Decision != types.DecisionManualReview {
		t.Errorf("Decision = %s, want MANUAL_REVIEW (confidence below threshold)", result.ReviewResult.Decision)
	}
}

func TestRun_AllowAutoRejectDisabled(t *testing.T) {
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeStopResponse("REJECTED", 0.95, nil), // high confidence, but auto-reject disabled
	}

	state := NewState(testAd())
	config := testConfig()
	config.AllowAutoReject = false
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), client, config, state, nil, testLogger())

	if result.ReviewResult.Decision != types.DecisionManualReview {
		t.Errorf("Decision = %s, want MANUAL_REVIEW (auto-reject disabled)", result.ReviewResult.Decision)
	}
}

func TestRun_TransitionLog(t *testing.T) {
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeToolCallResponse("analyze_content"),
		makeStopResponse("REJECTED", 0.92, nil),
	}

	state := NewState(testAd())
	config := testConfig()
	state.Messages = buildInitialMessages(config.SystemPrompt)

	Run(context.Background(), client, config, state, nil, testLogger())

	if len(state.TransitionLog) < 3 {
		t.Fatalf("TransitionLog has %d entries, want >= 3", len(state.TransitionLog))
	}

	// First transition: PENDING → ANALYZING (initialized)
	first := state.TransitionLog[0]
	if first.From != StatePending || first.To != StateAnalyzing {
		t.Errorf("first transition: %s → %s, want PENDING → ANALYZING", first.From, first.To)
	}
	if first.Reason != TransitionInitialized {
		t.Errorf("first reason = %s, want initialized", first.Reason)
	}

	// Last transition should be → DECIDED
	last := state.TransitionLog[len(state.TransitionLog)-1]
	if last.To != StateDecided {
		t.Errorf("last transition.To = %s, want DECIDED", last.To)
	}

	// Verify log is human-readable.
	log := state.FormatTransitionLog()
	if log == "(no transitions)" {
		t.Error("FormatTransitionLog should not be empty")
	}
	t.Logf("Transition log: %s", log)
}

func TestRun_EventEmission(t *testing.T) {
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeToolCallResponse("analyze_content"),
		makeStopResponse("PASSED", 0.90, nil),
	}

	state := NewState(testAd())
	config := testConfig()
	state.Messages = buildInitialMessages(config.SystemPrompt)

	events := make(chan StreamEvent, 64)
	var received []StreamEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			received = append(received, ev)
		}
	}()

	Run(context.Background(), client, config, state, events, testLogger())
	close(events)
	<-done

	if len(received) == 0 {
		t.Error("expected events to be emitted")
	}

	// Check that we got TurnStarted and TurnCompleted events.
	var turnStarted, turnCompleted int
	for _, ev := range received {
		switch ev.Type {
		case EventTurnStarted:
			turnStarted++
		case EventTurnCompleted:
			turnCompleted++
		}
	}
	if turnStarted == 0 {
		t.Error("expected TurnStarted events")
	}
	if turnCompleted == 0 {
		t.Error("expected TurnCompleted events")
	}
	t.Logf("Received %d events (%d starts, %d completes)", len(received), turnStarted, turnCompleted)
}

// --- extractJSON tests ---

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"raw json", `{"decision":"PASSED"}`, `{"decision":"PASSED"}`},
		{"fenced json", "```json\n{\"decision\":\"PASSED\"}\n```", `{"decision":"PASSED"}`},
		{"embedded json", "Here is my review:\n{\"decision\":\"REJECTED\"}\nEnd.", `{"decision":"REJECTED"}`},
		{"no json", "just plain text", ""},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSON(tt.input)
			if got != tt.want {
				t.Errorf("extractJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Phase 3 integration tests ---

// compactMockLLM serves both the main review loop and the compact summarization call.
// It returns responses in order; compact calls get the summary response.
type compactMockLLM struct {
	responses []*types.ChatCompletionResponse
	errors    []error
	idx       int
}

func (m *compactMockLLM) ChatCompletion(_ context.Context, _ types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	i := m.idx
	m.idx++
	if i < len(m.errors) && m.errors[i] != nil {
		return nil, m.errors[i]
	}
	if i < len(m.responses) {
		return m.responses[i], nil
	}
	return makeStopResponse("MANUAL_REVIEW", 0.5, nil), nil
}

func (m *compactMockLLM) StreamChatCompletion(_ context.Context, _ types.ChatCompletionRequest) (*llm.StreamReader, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *compactMockLLM) Usage() *llm.SessionUsage { return llm.NewSessionUsage() }

func TestRun_WithContextManager(t *testing.T) {
	// Set up a ContextManager with very small window so it triggers AutoCompact.
	compactLLM := &compactMockLLM{
		responses: []*types.ChatCompletionResponse{
			// Response 0: AutoCompact summary call.
			{Choices: []types.Choice{{
				Message: types.Message{
					Role: types.RoleAssistant,
					Content: types.NewTextContent(
						"<summary>Previous review: ad_test PASSED with 0.9 confidence. No violations found.</summary>"),
				},
				FinishReason: "stop",
			}}},
			// Response 1: main loop stop (after compact).
			makeStopResponse("PASSED", 0.88, nil),
		},
	}

	cfg := compact.CompactConfig{
		ContextWindowSize:      200, // very small → triggers AutoCompact
		AutoCompactBuffer:      20,
		SummaryOutputReserve:   20,
		MicroCompactKeepRecent: 2,
		MaxConsecutiveFailures: 3,
	}
	cm := compact.NewContextManager(cfg, compactLLM, testLogger())

	config := testConfig()
	config.ContextManager = cm

	// Build state with a large system prompt to exceed the small threshold.
	state := NewState(testAd())
	state.Messages = []types.Message{
		{Role: types.RoleSystem, Content: types.NewTextContent(strings.Repeat("x", 600))},
		{Role: types.RoleUser, Content: types.NewTextContent("Review this ad")},
	}

	result := Run(context.Background(), compactLLM, config, state, nil, testLogger())

	if result.ExitReason != ExitCompleted {
		t.Errorf("ExitReason = %s, want completed", result.ExitReason)
	}

	// Verify AutoCompact was triggered via TransitionLog.
	found := false
	for _, tr := range state.TransitionLog {
		if tr.Reason == TransitionAutoCompact {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected TransitionAutoCompact in log, got: %s", state.FormatTransitionLog())
	}
	if !state.HasAttemptedCompact {
		t.Error("expected HasAttemptedCompact = true")
	}
}

func TestRun_ReactiveCompactRecovery(t *testing.T) {
	// Scenario: first LLM call returns prompt_too_long → reactive compact → retry → success.
	compactLLM := &compactMockLLM{
		responses: []*types.ChatCompletionResponse{
			nil, // index 0: error (see errors below)
			// index 1: reactive compact summary call (must be >= 50 chars after extraction).
			{Choices: []types.Choice{{
				Message: types.Message{
					Role:    types.RoleAssistant,
					Content: types.NewTextContent("<summary>Compressed context for retry. Previously reviewed ad_test for healthcare compliance in US region with standard pipeline.</summary>"),
				},
				FinishReason: "stop",
			}}},
			// index 2: retry succeeds with stop.
			makeStopResponse("PASSED", 0.90, nil),
		},
		errors: []error{
			&llm.APIError{StatusCode: 400, Message: "prompt is too long"},
			nil,
			nil,
		},
	}

	cfg := compact.CompactConfig{
		ContextWindowSize:      100000, // large → no proactive compact
		AutoCompactBuffer:      13000,
		SummaryOutputReserve:   8000,
		MicroCompactKeepRecent: 6,
		MaxConsecutiveFailures: 3,
	}
	cm := compact.NewContextManager(cfg, compactLLM, testLogger())

	config := testConfig()
	config.ContextManager = cm

	state := NewState(testAd())
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), compactLLM, config, state, nil, testLogger())

	if result.ExitReason != ExitCompleted {
		t.Errorf("ExitReason = %s, want completed (reactive should recover)", result.ExitReason)
	}

	// Verify ReactiveCompact was triggered.
	found := false
	for _, tr := range state.TransitionLog {
		if tr.Reason == TransitionReactiveCompact {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected TransitionReactiveCompact in log, got: %s", state.FormatTransitionLog())
	}
	if !state.HasAttemptedCompact {
		t.Error("expected HasAttemptedCompact = true after reactive compact")
	}
}

func TestRun_BudgetExhausted(t *testing.T) {
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		// Response with high usage that exceeds budget.
		{
			Choices: []types.Choice{{
				Message:      types.Message{Role: types.RoleAssistant, Content: types.NewTextContent("analyzing...")},
				FinishReason: "tool_calls",
			}},
			// Trick: won't have ToolCalls so it'll go to default branch.
			// Actually, we need a stop response but with usage that triggers budget.
			// Let's use a stop response with usage.
			Usage: &types.Usage{PromptTokens: 5000, CompletionTokens: 5000},
		},
	}
	// Override: make it a stop response so it doesn't need tool calls.
	client.Responses[0].Choices[0].FinishReason = "stop"
	client.Responses[0].Choices[0].Message.Content = types.NewTextContent(
		`{"decision":"REJECTED","confidence":0.95,"violations":[],"reasoning":"test"}`)

	// Budget: 1000 tokens, threshold 0.9 → trips at 900.
	// Usage: 10000 → way over 900.
	tb := compact.NewTokenBudget(compact.BudgetConfig{
		MaxTokensPerReview:   1000,
		CompletionThreshold:  0.9,
		DiminishingThreshold: 500,
		DiminishingMinChecks: 10,
	})

	config := testConfig()
	config.TokenBudget = tb

	state := NewState(testAd())
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), client, config, state, nil, testLogger())

	// Budget should trip before the stop handler processes the response.
	if result.ExitReason != ExitCompleted {
		t.Errorf("ExitReason = %s, want completed (budget fallback)", result.ExitReason)
	}
	if result.ReviewResult == nil {
		t.Fatal("ReviewResult is nil")
	}
	if result.ReviewResult.Decision != types.DecisionManualReview {
		t.Errorf("Decision = %s, want MANUAL_REVIEW (budget fallback)", result.ReviewResult.Decision)
	}

	// Verify budget exhaustion in transition log.
	found := false
	for _, tr := range state.TransitionLog {
		if tr.Reason == TransitionBudgetExhausted {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected TransitionBudgetExhausted in log, got: %s", state.FormatTransitionLog())
	}
}
