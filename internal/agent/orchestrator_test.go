package agent

import (
	"context"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/agent/mock"
	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/tool"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// loadTestMatrix and testLogger are defined in engine_test.go and loop_test.go.

// mockMultiLLM returns different responses for different call indices,
// supporting multi-agent parallel execution.
type mockMultiLLM struct {
	callCount int
}

func (m *mockMultiLLM) ChatCompletion(_ context.Context, req types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	m.callCount++
	// All agents and adjudicator get a stop response with a valid review JSON.
	// Differentiate by checking if system prompt mentions specific roles.
	return makeStopResponse("REJECTED", 0.88, []types.PolicyViolation{
		{PolicyID: "POL_001", Severity: "critical", Description: "test violation", Confidence: 0.9},
	}), nil
}

func (m *mockMultiLLM) StreamChatCompletion(_ context.Context, _ types.ChatCompletionRequest) (*llm.StreamReader, error) {
	return nil, nil
}

func (m *mockMultiLLM) Usage() *llm.SessionUsage { return llm.NewSessionUsage() }

func TestOrchestrator_RunMultiAgent_Comprehensive(t *testing.T) {
	matrix := loadTestMatrix(t)
	logger := testLogger()
	mockLLM := &mockMultiLLM{}

	// Create real registry with mock LLM.
	reg := tool.NewReviewRegistry(mockLLM, matrix, logger)

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

	// Should have 3 specialist results.
	if len(result.AgentResults) != 3 {
		t.Errorf("expected 3 agent results, got %d", len(result.AgentResults))
	}

	// Verify agent roles.
	roles := map[AgentRole]bool{}
	for _, ar := range result.AgentResults {
		roles[ar.Role] = true
	}
	for _, expected := range []AgentRole{RoleContent, RolePolicy, RoleRegion} {
		if !roles[expected] {
			t.Errorf("missing agent role: %s", expected)
		}
	}

	// Should have a final ReviewResult.
	if result.ReviewResult == nil {
		t.Fatal("ReviewResult is nil")
	}

	// Chain log should have 4 entries (3 specialists + 1 adjudicator).
	if result.ChainLog == nil {
		t.Fatal("ChainLog is nil")
	}
	if len(result.ChainLog.Entries) != 4 {
		t.Errorf("expected 4 chain entries, got %d", len(result.ChainLog.Entries))
	}

	// All entries should share the same ChainID.
	chainID := result.ChainLog.ChainID
	for _, e := range result.ChainLog.Entries {
		if e.ChainID != chainID {
			t.Errorf("chain ID mismatch: %s vs %s", e.ChainID, chainID)
		}
	}

	// Specialist agents at depth 1.
	for _, e := range result.ChainLog.Entries[:3] {
		if e.Depth != 1 {
			t.Errorf("specialist depth should be 1, got %d for %s", e.Depth, e.AgentRole)
		}
	}

	// LLM was called: at least 3 specialists + 1 adjudicator + tool calls.
	if mockLLM.callCount < 4 {
		t.Errorf("expected at least 4 LLM calls, got %d", mockLLM.callCount)
	}

	t.Logf("Chain:\n%s", result.ChainLog.Format())
}

func TestOrchestrator_SpecialistToolRestriction(t *testing.T) {
	matrix := loadTestMatrix(t)
	logger := testLogger()

	reg := tool.NewReviewRegistry(&mockMultiLLM{}, matrix, logger)

	// Content agent should only have 2 tools.
	contentReg := reg.Sub("analyze_content", "match_policies")
	contentDefs := contentReg.ExportDefinitions()
	if len(contentDefs) != 2 {
		t.Errorf("content agent should have 2 tools, got %d", len(contentDefs))
	}

	// Adjudicator should have 0 tools.
	adjReg := reg.Sub() // empty
	adjDefs := adjReg.ExportDefinitions()
	if len(adjDefs) != 0 {
		t.Errorf("adjudicator should have 0 tools, got %d", len(adjDefs))
	}
}

func TestOrchestrator_FastPipelineSingleAgent(t *testing.T) {
	// Fast pipeline should NOT trigger multi-agent.
	matrix := loadTestMatrix(t)
	logger := testLogger()
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{
		makeStopResponse("PASSED", 0.90, nil),
	}

	reg := tool.NewReviewRegistry(client, matrix, logger)
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

	// Fast pipeline → single agent → should complete normally.
	if result.ExitReason != ExitCompleted {
		t.Errorf("ExitReason = %s, want completed", result.ExitReason)
	}
	if result.ReviewResult == nil {
		t.Fatal("ReviewResult is nil")
	}
	// Should be PASSED (single agent, low risk).
	if result.ReviewResult.Decision != types.DecisionPassed {
		t.Errorf("Decision = %s, want PASSED for fast pipeline", result.ReviewResult.Decision)
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
	cl.Add(ChainEntry{ChainID: "test-chain-id", Depth: 0, AgentRole: "orchestrator", Decision: "N/A"})
	cl.Add(ChainEntry{ChainID: "test-chain-id", Depth: 1, AgentRole: "content", Decision: "REJECTED", Confidence: 0.9})
	cl.Add(ChainEntry{ChainID: "test-chain-id", Depth: 1, AgentRole: "adjudicator", Decision: "REJECTED", Confidence: 0.88})

	output := cl.Format()
	if output == "" || output == "(empty chain)" {
		t.Error("expected non-empty chain format")
	}
	t.Logf("Chain format:\n%s", output)
}
