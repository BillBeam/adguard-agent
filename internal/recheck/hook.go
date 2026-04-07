package recheck

import (
	"time"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// RecheckHook implements agent.PostReviewHook. It schedules a recheck for
// PASSED ads in high/critical risk categories. The riskLookup function is
// injected for testability (in production: strategy.StrategyMatrix.GetRiskLevel).
type RecheckHook struct {
	scheduler  *RecheckScheduler
	riskLookup func(category string) types.RiskLevel
	delay      time.Duration
}

// NewRecheckHook creates a hook that auto-schedules rechecks for high-risk PASSED ads.
func NewRecheckHook(scheduler *RecheckScheduler, riskLookup func(string) types.RiskLevel, delay time.Duration) *RecheckHook {
	return &RecheckHook{
		scheduler:  scheduler,
		riskLookup: riskLookup,
		delay:      delay,
	}
}

// PostReview implements agent.PostReviewHook. Only schedules rechecks for
// PASSED ads with high or critical risk level — low/medium risk PASSED ads
// are not worth the cost of a follow-up review.
func (h *RecheckHook) PostReview(result types.ReviewResult, _, _, category, _ string) {
	if result.Decision != types.DecisionPassed {
		return
	}
	risk := h.riskLookup(category)
	if risk != types.RiskHigh && risk != types.RiskCritical {
		return
	}
	// Error is non-fatal: if a recheck is already scheduled, skip silently.
	h.scheduler.Schedule(result.AdID, h.delay, "high-risk PASSED ad: post-approval recheck")
}
