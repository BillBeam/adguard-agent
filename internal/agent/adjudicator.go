package agent

import "github.com/BillBeam/adguard-agent/internal/types"

// Adjudication logic — 误伤控制 L3: Multi-Agent 交叉验证。
//
// Business alignment: 4-layer false-positive control system:
//   L1: Memory consistency (HistoryLookup, Phase 2)
//   L2: Confidence threshold routing (Phase 1)
//   L3: Multi-Agent cross-validation (Phase 4, THIS FILE)
//   L4: Verification re-check (Phase 3)
//
// L3 rules:
//   - ALL agents agree → high confidence, maintain decision
//   - 2:1 split → follow majority, reduce confidence
//   - 3-way disagreement → MANUAL_REVIEW (too complex for automation)
//   - ANY agent found critical violation → at minimum MANUAL_REVIEW

// applyL3Control applies Multi-Agent cross-validation rules.
// This runs AFTER the Adjudicator's LLM-based decision, as a programmatic safety net.
func applyL3Control(results []AgentResult, decision types.ReviewDecision, confidence float64) (types.ReviewDecision, float64) {
	if len(results) <= 1 {
		return decision, confidence
	}

	// Check for critical violations from any agent.
	hasCritical := false
	for _, ar := range results {
		for _, v := range ar.Violations {
			if v.Severity == "critical" {
				hasCritical = true
				break
			}
		}
	}

	// Rule 0 (highest priority): critical violation override.
	// ANY agent finding a critical violation prevents PASSED — fail-closed.
	if hasCritical && decision == types.DecisionPassed {
		return types.DecisionManualReview, confidence * 0.6
	}

	// Count decisions.
	counts := map[string]int{}
	for _, ar := range results {
		counts[ar.Decision]++
	}

	total := len(results)
	unanimousDecision := ""
	for d, c := range counts {
		if c == total {
			unanimousDecision = d
		}
	}

	// Rule 1: ALL agents agree → high confidence, maintain decision.
	if unanimousDecision != "" {
		boosted := confidence
		if boosted < 0.95 {
			boosted = confidence + 0.05
			if boosted > 1.0 {
				boosted = 1.0
			}
		}
		return decision, boosted
	}

	// Rule 2: 3-way disagreement → MANUAL_REVIEW.
	if len(counts) >= 3 {
		if confidence > 0.5 {
			confidence = 0.5
		}
		return types.DecisionManualReview, confidence
	}

	// Rule 3: 2:1 split → follow majority but reduce confidence.
	majorityDecision := ""
	majorityCount := 0
	for d, c := range counts {
		if c > majorityCount {
			majorityDecision = d
			majorityCount = c
		}
	}

	if majorityCount > total/2 {
		reduced := confidence * 0.85
		d := types.ReviewDecision(majorityDecision)

		// If minority says REJECTED and majority says PASSED → at least MANUAL_REVIEW.
		if d == types.DecisionPassed && counts[string(types.DecisionRejected)] > 0 {
			d = types.DecisionManualReview
			reduced = confidence * 0.7
		}

		return d, reduced
	}

	return decision, confidence
}

// aggregateDecisions is the programmatic fallback when Adjudicator fails.
// Uses simple majority voting + L3 rules.
func aggregateDecisions(results []AgentResult) (types.ReviewDecision, float64) {
	if len(results) == 0 {
		return types.DecisionManualReview, 0.0
	}

	counts := map[string]int{}
	var totalConf float64
	for _, ar := range results {
		counts[ar.Decision]++
		totalConf += ar.Confidence
	}
	avgConf := totalConf / float64(len(results))

	// Find majority.
	majorityDecision := string(types.DecisionManualReview)
	majorityCount := 0
	for d, c := range counts {
		if c > majorityCount {
			majorityDecision = d
			majorityCount = c
		}
	}

	// No clear majority → MANUAL_REVIEW.
	if majorityCount <= len(results)/2 {
		return types.DecisionManualReview, avgConf * 0.5
	}

	return types.ReviewDecision(majorityDecision), avgConf
}
