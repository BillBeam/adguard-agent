package agent

import (
	"testing"

	"github.com/BillBeam/adguard-agent/internal/types"
)

func TestL3Control_AllAgree_Rejected(t *testing.T) {
	results := []AgentResult{
		{Role: RoleContent, Decision: "REJECTED", Confidence: 0.90},
		{Role: RolePolicy, Decision: "REJECTED", Confidence: 0.85},
		{Role: RoleRegion, Decision: "REJECTED", Confidence: 0.88},
	}
	decision, conf := applyL3Control(results, types.DecisionRejected, 0.88)
	if decision != types.DecisionRejected {
		t.Errorf("unanimous REJECTED should maintain, got %s", decision)
	}
	// Confidence should be boosted slightly.
	if conf <= 0.88 {
		t.Errorf("unanimous agreement should boost confidence, got %.2f", conf)
	}
}

func TestL3Control_AllAgree_Passed(t *testing.T) {
	results := []AgentResult{
		{Role: RoleContent, Decision: "PASSED", Confidence: 0.92},
		{Role: RolePolicy, Decision: "PASSED", Confidence: 0.90},
		{Role: RoleRegion, Decision: "PASSED", Confidence: 0.88},
	}
	decision, _ := applyL3Control(results, types.DecisionPassed, 0.90)
	if decision != types.DecisionPassed {
		t.Errorf("unanimous PASSED should maintain, got %s", decision)
	}
}

func TestL3Control_TwoOneeSplit_MajorityRejected(t *testing.T) {
	results := []AgentResult{
		{Role: RoleContent, Decision: "REJECTED", Confidence: 0.90},
		{Role: RolePolicy, Decision: "REJECTED", Confidence: 0.85},
		{Role: RoleRegion, Decision: "MANUAL_REVIEW", Confidence: 0.60},
	}
	decision, conf := applyL3Control(results, types.DecisionRejected, 0.85)
	// 2:1 split with majority REJECTED → maintain REJECTED but reduce confidence.
	if decision != types.DecisionRejected {
		t.Errorf("2:1 majority REJECTED should maintain, got %s", decision)
	}
	if conf >= 0.85 {
		t.Errorf("split should reduce confidence, got %.2f", conf)
	}
}

func TestL3Control_TwoOneSplit_MajorityPassed_MinorityRejected(t *testing.T) {
	results := []AgentResult{
		{Role: RoleContent, Decision: "PASSED", Confidence: 0.85},
		{Role: RolePolicy, Decision: "PASSED", Confidence: 0.82},
		{Role: RoleRegion, Decision: "REJECTED", Confidence: 0.70},
	}
	decision, _ := applyL3Control(results, types.DecisionPassed, 0.83)
	// 2 PASSED + 1 REJECTED → safety net: at least MANUAL_REVIEW.
	if decision != types.DecisionManualReview {
		t.Errorf("PASSED majority with REJECTED minority → MANUAL_REVIEW, got %s", decision)
	}
}

func TestL3Control_ThreeWayDisagreement(t *testing.T) {
	results := []AgentResult{
		{Role: RoleContent, Decision: "REJECTED", Confidence: 0.80},
		{Role: RolePolicy, Decision: "PASSED", Confidence: 0.75},
		{Role: RoleRegion, Decision: "MANUAL_REVIEW", Confidence: 0.60},
	}
	decision, conf := applyL3Control(results, types.DecisionRejected, 0.75)
	// 3-way disagreement → MANUAL_REVIEW.
	if decision != types.DecisionManualReview {
		t.Errorf("3-way disagreement should force MANUAL_REVIEW, got %s", decision)
	}
	if conf > 0.5 {
		t.Errorf("3-way disagreement should cap confidence at 0.5, got %.2f", conf)
	}
}

func TestL3Control_CriticalViolation_OverridesPassed(t *testing.T) {
	results := []AgentResult{
		{Role: RoleContent, Decision: "PASSED", Confidence: 0.90, Violations: []types.PolicyViolation{
			{PolicyID: "POL_001", Severity: "critical"},
		}},
		{Role: RolePolicy, Decision: "PASSED", Confidence: 0.85},
		{Role: RoleRegion, Decision: "PASSED", Confidence: 0.88},
	}
	decision, _ := applyL3Control(results, types.DecisionPassed, 0.88)
	// Critical violation + PASSED → at least MANUAL_REVIEW.
	if decision == types.DecisionPassed {
		t.Error("critical violation should prevent PASSED, expected MANUAL_REVIEW or REJECTED")
	}
}

func TestL3Control_SingleAgent(t *testing.T) {
	results := []AgentResult{
		{Role: RoleContent, Decision: "REJECTED", Confidence: 0.90},
	}
	// With single agent, L3 should pass through (no cross-validation possible).
	decision, conf := applyL3Control(results, types.DecisionRejected, 0.90)
	if decision != types.DecisionRejected {
		t.Errorf("single agent should pass through, got %s", decision)
	}
	if conf != 0.90 {
		t.Errorf("single agent should not change confidence, got %.2f", conf)
	}
}

func TestAggregateDecisions_Majority(t *testing.T) {
	results := []AgentResult{
		{Decision: "REJECTED", Confidence: 0.90},
		{Decision: "REJECTED", Confidence: 0.85},
		{Decision: "PASSED", Confidence: 0.70},
	}
	decision, _ := aggregateDecisions(results)
	if decision != types.DecisionRejected {
		t.Errorf("expected REJECTED majority, got %s", decision)
	}
}

func TestAggregateDecisions_NoMajority(t *testing.T) {
	results := []AgentResult{
		{Decision: "REJECTED", Confidence: 0.80},
		{Decision: "PASSED", Confidence: 0.75},
		{Decision: "MANUAL_REVIEW", Confidence: 0.60},
	}
	decision, _ := aggregateDecisions(results)
	if decision != types.DecisionManualReview {
		t.Errorf("no majority should fallback to MANUAL_REVIEW, got %s", decision)
	}
}

func TestAggregateDecisions_Empty(t *testing.T) {
	decision, conf := aggregateDecisions(nil)
	if decision != types.DecisionManualReview || conf != 0.0 {
		t.Errorf("empty results should be MANUAL_REVIEW/0.0, got %s/%.2f", decision, conf)
	}
}
