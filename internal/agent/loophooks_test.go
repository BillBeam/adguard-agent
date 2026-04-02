package agent

import (
	"context"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/agent/mock"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// --- ToolPermissionHook tests ---

func TestToolPermissionHook_AllowedTool(t *testing.T) {
	h := NewToolPermissionHook([]string{"analyze_content", "match_policies"}, testLogger())
	if err := h.PreToolExec("analyze_content", nil); err != nil {
		t.Errorf("allowed tool should pass, got: %v", err)
	}
}

func TestToolPermissionHook_BlockedTool(t *testing.T) {
	h := NewToolPermissionHook([]string{"analyze_content"}, testLogger())
	if err := h.PreToolExec("lookup_history", nil); err == nil {
		t.Error("blocked tool should return error")
	}
}

func TestToolPermissionHook_EmptyAllowsAll(t *testing.T) {
	h := NewToolPermissionHook(nil, testLogger())
	if err := h.PreToolExec("anything", nil); err != nil {
		t.Errorf("empty config should allow all, got: %v", err)
	}
}

// --- AuditHook tests ---

func TestAuditHook_RecordsPreAndPost(t *testing.T) {
	h := NewAuditHook(testLogger())
	h.PreToolExec("analyze_content", nil)
	h.PostToolExec("analyze_content", `{"signals":[]}`, nil)

	entries := h.Entries()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Phase != "pre" || entries[0].ToolName != "analyze_content" {
		t.Errorf("first entry: got phase=%s tool=%s", entries[0].Phase, entries[0].ToolName)
	}
	if entries[1].Phase != "post" {
		t.Errorf("second entry: got phase=%s", entries[1].Phase)
	}
}

// --- CircuitBreakerHook tests ---

func TestCircuitBreakerHook_TripsAfterThreshold(t *testing.T) {
	h := NewCircuitBreakerHook(3, testLogger())

	// 3 consecutive failures.
	for i := 0; i < 3; i++ {
		h.PostToolExec("tool", `{"error":"fail"}`, nil)
	}

	if !h.IsTripped() {
		t.Error("breaker should be tripped after 3 failures")
	}

	// PreToolExec should block.
	if err := h.PreToolExec("any_tool", nil); err == nil {
		t.Error("tripped breaker should block tool calls")
	}
}

func TestCircuitBreakerHook_ResetsOnSuccess(t *testing.T) {
	h := NewCircuitBreakerHook(3, testLogger())

	h.PostToolExec("tool", `{"error":"fail"}`, nil)
	h.PostToolExec("tool", `{"error":"fail"}`, nil)
	h.PostToolExec("tool", `{"signals":[]}`, nil) // success resets

	if h.IsTripped() {
		t.Error("breaker should not be tripped after success reset")
	}
}

// --- ResultValidationHook tests ---

func TestResultValidationHook_ValidResult(t *testing.T) {
	h := NewResultValidationHook(testLogger())
	state := NewState(testAd())
	state.AppendTrace("tool_call:analyze_content")

	if err := h.BeforeStop(state, ExitCompleted); err != nil {
		t.Errorf("valid result should pass, got: %v", err)
	}
}

// --- FinalAuditHook tests ---

func TestFinalAuditHook_NoError(t *testing.T) {
	h := NewFinalAuditHook(testLogger())
	state := NewState(testAd())
	if err := h.BeforeStop(state, ExitCompleted); err != nil {
		t.Errorf("audit hook should not error, got: %v", err)
	}
}

// --- Hook panic recovery tests ---

type panickingPreHook struct{}

func (h *panickingPreHook) PreToolExec(_ string, _ []byte) error {
	panic("pre-hook panic")
}

func TestRunPreToolHooks_PanicRecovery(t *testing.T) {
	hooks := []PreToolHook{&panickingPreHook{}}
	err := runPreToolHooks(hooks, "test", nil, testLogger())
	if err == nil {
		t.Error("panic should be recovered as error")
	}
}

type panickingPostHook struct{}

func (h *panickingPostHook) PostToolExec(_ string, _ string, _ error) {
	panic("post-hook panic")
}

func TestRunPostToolHooks_PanicRecovery(t *testing.T) {
	hooks := []PostToolHook{&panickingPostHook{}}
	// Should not panic.
	runPostToolHooks(hooks, "test", "", nil, testLogger())
}

type panickingStopHook struct{}

func (h *panickingStopHook) BeforeStop(_ *State, _ ExitReason) error {
	panic("stop-hook panic")
}

func TestRunStopHooks_PanicRecovery(t *testing.T) {
	hooks := []StopHook{&panickingStopHook{}}
	state := NewState(testAd())
	// Should not panic.
	runStopHooks(hooks, state, ExitCompleted, testLogger())
}

// --- Integration: PreToolHook blocking in loop ---

func TestRun_WithPreToolHookBlocking(t *testing.T) {
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeToolCallResponse("analyze_content", "lookup_history"),
		makeStopResponse("PASSED", 0.85, nil),
	}

	config := testConfig()
	// Only allow analyze_content, block lookup_history.
	config.PreToolHooks = []PreToolHook{
		NewToolPermissionHook([]string{"analyze_content", "match_policies", "check_landing_page"}, testLogger()),
	}

	state := NewState(testAd())
	state.Messages = buildInitialMessages(config.SystemPrompt)

	result := Run(context.Background(), client, config, state, nil, testLogger())

	if result.ExitReason != ExitCompleted {
		t.Fatalf("expected completed, got %s", result.ExitReason)
	}

	// Verify lookup_history was blocked.
	blocked := false
	for _, tr := range state.PartialResult.AgentTrace {
		if tr == "tool_blocked:lookup_history" {
			blocked = true
		}
	}
	if !blocked {
		t.Error("lookup_history should have been blocked by PreToolHook")
	}
}

// --- Integration: StopHook fires on all exit paths ---

type recordingStopHook struct {
	called bool
	reason ExitReason
}

func (h *recordingStopHook) BeforeStop(_ *State, reason ExitReason) error {
	h.called = true
	h.reason = reason
	return nil
}

func TestRun_StopHook_FiresOnCompletion(t *testing.T) {
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeStopResponse("PASSED", 0.90, nil),
	}

	hook := &recordingStopHook{}
	config := testConfig()
	config.StopHooks = []StopHook{hook}

	state := NewState(testAd())
	state.Messages = buildInitialMessages(config.SystemPrompt)

	Run(context.Background(), client, config, state, nil, testLogger())

	if !hook.called {
		t.Error("StopHook should have been called")
	}
	if hook.reason != ExitCompleted {
		t.Errorf("expected ExitCompleted, got %s", hook.reason)
	}
}

func TestRun_StopHook_FiresOnMaxTurns(t *testing.T) {
	client := mock.NewLLMClient()
	// Always return tool calls → will hit max turns.
	client.Responses = []*types.ChatCompletionResponse{
		makeToolCallResponse("analyze_content"),
		makeToolCallResponse("analyze_content"),
		makeToolCallResponse("analyze_content"),
		makeToolCallResponse("analyze_content"),
		makeToolCallResponse("analyze_content"),
		makeToolCallResponse("analyze_content"),
	}

	hook := &recordingStopHook{}
	config := testConfig()
	config.MaxTurns = 2
	config.StopHooks = []StopHook{hook}

	state := NewState(testAd())
	state.Messages = buildInitialMessages(config.SystemPrompt)

	Run(context.Background(), client, config, state, nil, testLogger())

	if !hook.called {
		t.Error("StopHook should fire on max turns")
	}
	if hook.reason != ExitMaxTurns {
		t.Errorf("expected ExitMaxTurns, got %s", hook.reason)
	}
}
