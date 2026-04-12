package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/agent/mock"
	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/tool"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// loadTestMatrix and testLogger are defined in engine_test.go and loop_test.go.

// coordinatorMockLLM simulates the Coordinator-driven orchestration:
// - Coordinator's first call: returns tool_calls to dispatch all 3 specialists
// - Specialist calls: return stop with REJECTED
// - Coordinator's second call (after seeing results): returns final decision
type coordinatorMockLLM struct {
	callCount int
}

func (m *coordinatorMockLLM) ChatCompletion(_ context.Context, req types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	m.callCount++

	// Detect if this is the Coordinator by checking for dispatch_specialist in tools.
	isCoordinator := false
	for _, t := range req.Tools {
		if t.Function.Name == "dispatch_specialist" {
			isCoordinator = true
			break
		}
	}

	if isCoordinator {
		// First coordinator call: dispatch all 3 specialists.
		if m.callCount <= 1 {
			return &types.ChatCompletionResponse{
				Choices: []types.Choice{{
					Message: types.Message{
						Role: types.RoleAssistant,
						ToolCalls: []types.ToolCall{
							{ID: "call_1", Type: "function", Function: types.ToolCallFunction{Name: "dispatch_specialist", Arguments: json.RawMessage(`{"role":"content"}`)}},
							{ID: "call_2", Type: "function", Function: types.ToolCallFunction{Name: "dispatch_specialist", Arguments: json.RawMessage(`{"role":"policy"}`)}},
							{ID: "call_3", Type: "function", Function: types.ToolCallFunction{Name: "dispatch_specialist", Arguments: json.RawMessage(`{"role":"region"}`)}},
						},
					},
					FinishReason: "tool_calls",
				}},
			}, nil
		}

		// Subsequent coordinator call: produce final decision.
		return makeStopResponse("REJECTED", 0.92, []types.PolicyViolation{
			{PolicyID: "POL_001", Severity: "critical", Description: "coordinator synthesis", Confidence: 0.92},
		}), nil
	}

	// Specialist calls: simple stop with REJECTED.
	return makeStopResponse("REJECTED", 0.88, []types.PolicyViolation{
		{PolicyID: "POL_001", Severity: "critical", Description: "test violation", Confidence: 0.9},
	}), nil
}

func (m *coordinatorMockLLM) StreamChatCompletion(_ context.Context, _ types.ChatCompletionRequest) (*llm.StreamReader, error) {
	return nil, nil
}

func (m *coordinatorMockLLM) Usage() *llm.SessionUsage { return llm.NewSessionUsage() }

func TestOrchestrator_RunMultiAgent_CoordinatorDriven(t *testing.T) {
	matrix := loadTestMatrix(t)
	logger := testLogger()
	mockLLM := &coordinatorMockLLM{}

	reg := tool.NewReviewRegistry(mockLLM, matrix, nil, logger)
	orch := NewOrchestrator(mockLLM, matrix, reg, logger)

	ad := &types.AdContent{
		ID: "ad_test_multi", Type: "text", Region: "MENA_SA", Category: "healthcare",
		AdvertiserID: "adv_001",
		Content:      types.AdBody{Headline: "Miracle Cure", Body: "100% effective"},
		LandingPage:  types.LandingPage{URL: "https://example.com", IsAccessible: true},
	}
	plan := matrix.GetReviewPlan(ad.Region, ad.Category)

	result, err := orch.RunMultiAgent(context.Background(), ad, plan)
	if err != nil {
		t.Fatalf("RunMultiAgent failed: %v", err)
	}

	// Coordinator should have dispatched 3 specialists.
	if len(result.AgentResults) != 3 {
		t.Errorf("expected 3 agent results, got %d", len(result.AgentResults))
	}

	// Verify all specialist roles present.
	roles := map[AgentRole]bool{}
	for _, ar := range result.AgentResults {
		roles[ar.Role] = true
	}
	for _, expected := range []AgentRole{RoleContent, RolePolicy, RoleRegion} {
		if !roles[expected] {
			t.Errorf("missing agent role: %s", expected)
		}
	}

	// Final decision should exist.
	if result.ReviewResult == nil {
		t.Fatal("ReviewResult is nil")
	}

	// Chain log should have specialist entries.
	if result.ChainLog == nil {
		t.Fatal("ChainLog is nil")
	}
	if len(result.ChainLog.Entries) < 3 {
		t.Errorf("expected at least 3 chain entries, got %d", len(result.ChainLog.Entries))
	}

	// LLM was called: coordinator + 3 specialists.
	if mockLLM.callCount < 4 {
		t.Errorf("expected at least 4 LLM calls, got %d", mockLLM.callCount)
	}

	t.Logf("Chain:\n%s", result.ChainLog.Format())
	t.Logf("Decision: %s conf=%.2f", result.ReviewResult.Decision, result.ReviewResult.Confidence)
}

func TestOrchestrator_SpecialistToolRestriction(t *testing.T) {
	matrix := loadTestMatrix(t)
	logger := testLogger()

	reg := tool.NewReviewRegistry(&coordinatorMockLLM{}, matrix, nil, logger)

	// Content agent should only have 2 tools (new assignment: analyze_content + check_landing_page).
	contentReg := reg.Sub("analyze_content", "check_landing_page")
	contentDefs := contentReg.ExportDefinitions()
	if len(contentDefs) != 2 {
		t.Errorf("content agent should have 2 tools, got %d", len(contentDefs))
	}

	// Empty sub-registry should have 0 tools.
	emptyReg := reg.Sub()
	emptyDefs := emptyReg.ExportDefinitions()
	if len(emptyDefs) != 0 {
		t.Errorf("empty sub-registry should have 0 tools, got %d", len(emptyDefs))
	}
}

func TestOrchestrator_FastPipelineSingleAgent(t *testing.T) {
	matrix := loadTestMatrix(t)
	logger := testLogger()
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeStopResponse("PASSED", 0.90, nil),
	}

	reg := tool.NewReviewRegistry(client, matrix, nil, logger)
	executor := tool.NewExecutor(reg, logger)
	orch := NewOrchestrator(client, matrix, reg, logger)

	engine := NewReviewEngine(client, matrix, reg.ExportDefinitions(), executor, logger, nil)
	engine.WithOrchestrator(orch)

	ad := &types.AdContent{
		ID: "ad_fast", Type: "text", Region: "US", Category: "ecommerce",
		AdvertiserID: "adv_002",
		Content:      types.AdBody{Headline: "Summer Sale", Body: "50% off"},
		LandingPage:  types.LandingPage{URL: "https://shop.com", IsAccessible: true},
	}

	result, err := engine.Review(context.Background(), ad)
	if err != nil {
		t.Fatalf("Review failed: %v", err)
	}

	if result.ExitReason != ExitCompleted {
		t.Errorf("ExitReason = %s, want completed", result.ExitReason)
	}
	if result.ReviewResult == nil {
		t.Fatal("ReviewResult is nil")
	}
	if result.ReviewResult.Decision != types.DecisionPassed {
		t.Errorf("Decision = %s, want PASSED for fast pipeline", result.ReviewResult.Decision)
	}
}

func TestOrchestrator_CoordinatorPrompt_ContainsDispatch(t *testing.T) {
	ad := &types.AdContent{
		ID: "ad_test", Region: "US", Category: "healthcare",
		Content: types.AdBody{Headline: "Test"},
	}
	plan := types.ReviewPlan{Pipeline: "standard", ConfidenceThreshold: 0.75}
	prompt := BuildCoordinatorPrompt(ad, nil, plan, "")

	if !strings.Contains(prompt, "dispatch_specialist") {
		t.Error("coordinator prompt should mention dispatch_specialist")
	}
	if !strings.Contains(prompt, "content") || !strings.Contains(prompt, "policy") || !strings.Contains(prompt, "region") {
		t.Error("coordinator prompt should list available specialist roles")
	}
	if !strings.Contains(prompt, "MANUAL_REVIEW") {
		t.Error("coordinator prompt should mention fail-closed")
	}
}

func TestQueryChain_ChildDepth(t *testing.T) {
	root := NewQueryChain()
	if root.Depth != 0 {
		t.Errorf("root depth = %d, want 0", root.Depth)
	}

	child := root.Child()
	if child.Depth != 1 {
		t.Errorf("child depth = %d, want 1", child.Depth)
	}
	if child.ChainID != root.ChainID {
		t.Error("child should inherit parent's ChainID")
	}

	grandchild := child.Child()
	if grandchild.Depth != 2 {
		t.Errorf("grandchild depth = %d, want 2", grandchild.Depth)
	}
}

func TestChainLog_Format(t *testing.T) {
	cl := NewChainLog("test-chain-id")
	cl.Add(ChainEntry{ChainID: "test-chain-id", Depth: 0, AgentRole: "coordinator", Decision: "N/A"})
	cl.Add(ChainEntry{ChainID: "test-chain-id", Depth: 1, AgentRole: "content", Decision: "REJECTED", Confidence: 0.9})

	output := cl.Format()
	if output == "" || output == "(empty chain)" {
		t.Error("expected non-empty chain format")
	}
	t.Logf("Chain format:\n%s", output)
}
