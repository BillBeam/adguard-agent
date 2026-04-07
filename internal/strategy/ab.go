// A/B testing: automated comparison of canary vs active strategy versions.
//
// The comparison uses per-version review metrics computed from ReviewStore data
// (query-side aggregation, not write-time pre-computation). This matches the
// pattern where experiment analysis happens at read time using collected outcomes.
//
// The system recommends ROLLBACK/PROMOTE/CONTINUE but does not auto-execute —
// the operator makes the final decision. This is intentional: automated rollout
// decisions require higher confidence than we can guarantee with small sample sizes
// typical in ad review canary periods.
package strategy

import (
	"fmt"
	"log/slog"

	"github.com/BillBeam/adguard-agent/internal/store"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// ABRecommendation is the outcome of an A/B comparison.
type ABRecommendation string

const (
	ABRecommendRollback ABRecommendation = "ROLLBACK" // canary is worse, revert
	ABRecommendPromote  ABRecommendation = "PROMOTE"  // canary is better, graduate
	ABRecommendContinue ABRecommendation = "CONTINUE" // insufficient data or inconclusive
)

// ABComparison holds the result of comparing active vs canary strategy versions.
type ABComparison struct {
	ActiveVersion  string               `json:"active_version"`
	CanaryVersion  string               `json:"canary_version"`
	ActiveStats    store.ReviewStats    `json:"active_stats"`
	CanaryStats    store.ReviewStats    `json:"canary_stats"`
	Recommendation ABRecommendation     `json:"recommendation"`
	Reason         string               `json:"reason"`
}

// ABConfig holds thresholds for A/B comparison decisions.
type ABConfig struct {
	// FalsePositiveRollbackRatio: rollback if canary FP rate exceeds this multiple of active.
	FalsePositiveRollbackRatio float64
	// MinSampleSize: minimum canary reviews before any promote/rollback decision.
	MinSampleSize int
}

// DefaultABConfig returns conservative defaults.
func DefaultABConfig() ABConfig {
	return ABConfig{
		FalsePositiveRollbackRatio: 2.0,
		MinSampleSize:              10,
	}
}

// Compare evaluates canary vs active performance using ReviewStore data.
// Returns a recommendation with supporting metrics.
func Compare(
	vm *VersionManager,
	rs *store.ReviewStore,
	cfg ABConfig,
	logger *slog.Logger,
) (*ABComparison, error) {
	active, ok := vm.GetActive()
	if !ok {
		return nil, fmt.Errorf("no active version found")
	}
	canary, ok := vm.GetCanary()
	if !ok {
		return nil, fmt.Errorf("no canary version deployed")
	}

	activeStats := rs.VersionStats(active.VersionID)
	canaryStats := rs.VersionStats(canary.VersionID)

	comp := &ABComparison{
		ActiveVersion: active.VersionID,
		CanaryVersion: canary.VersionID,
		ActiveStats:   activeStats,
		CanaryStats:   canaryStats,
	}

	// Always update version metrics (even on early decisions).
	defer func() {
		vm.UpdateMetrics(active.VersionID, statsToMetrics(activeStats))
		vm.UpdateMetrics(canary.VersionID, statsToMetrics(canaryStats))
		if logger != nil {
			logger.Info("A/B comparison completed",
				slog.String("active", active.VersionID),
				slog.String("canary", canary.VersionID),
				slog.String("recommendation", string(comp.Recommendation)),
			)
		}
	}()

	// Decision logic.
	if canaryStats.Total < cfg.MinSampleSize {
		comp.Recommendation = ABRecommendContinue
		comp.Reason = fmt.Sprintf("insufficient canary data (%d reviews, need %d)",
			canaryStats.Total, cfg.MinSampleSize)
		return comp, nil
	}

	canaryFPRate := fpRate(canaryStats)
	activeFPRate := fpRate(activeStats)

	// Rule 1: Rollback if canary FP rate is significantly worse.
	if activeStats.Total > 0 && canaryFPRate > cfg.FalsePositiveRollbackRatio*activeFPRate && canaryFPRate > 0 {
		comp.Recommendation = ABRecommendRollback
		comp.Reason = fmt.Sprintf("canary FP rate %.1f%% exceeds %.1fx active FP rate %.1f%%",
			canaryFPRate*100, cfg.FalsePositiveRollbackRatio, activeFPRate*100)
		return comp, nil
	}

	// Rule 2: Promote if canary metrics are at least as good.
	if canaryStats.AverageConfidence >= activeStats.AverageConfidence && canaryFPRate <= activeFPRate {
		comp.Recommendation = ABRecommendPromote
		comp.Reason = fmt.Sprintf("canary metrics equal or better (conf=%.2f vs %.2f, FP=%.1f%% vs %.1f%%)",
			canaryStats.AverageConfidence, activeStats.AverageConfidence, canaryFPRate*100, activeFPRate*100)
		return comp, nil
	}

	// Default: continue observing.
	comp.Recommendation = ABRecommendContinue
	comp.Reason = "metrics inconclusive, continue collecting data"
	return comp, nil
}

// fpRate computes false positive rate: overrides / total (0 if total is 0).
func fpRate(s store.ReviewStats) float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.FalsePositiveCount) / float64(s.Total)
}

// statsToMetrics converts ReviewStats to StrategyMetrics for version tracking.
func statsToMetrics(s store.ReviewStats) types.StrategyMetrics {
	return types.StrategyMetrics{
		Accuracy:          s.AverageConfidence, // proxy
		FalsePositiveRate: fpRate(s),
	}
}
