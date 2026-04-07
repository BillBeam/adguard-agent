package strategy

import (
	"log/slog"
	"os"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/store"
	"github.com/BillBeam/adguard-agent/internal/types"
)

func abLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func setupABTest(t *testing.T) (*VersionManager, *store.ReviewStore) {
	t.Helper()
	vm := NewVersionManager(abLogger())
	vm.Create("v1.0")
	vm.Deploy("v1.0", 0)
	vm.Promote("v1.0")
	vm.Create("v2.0")
	vm.Deploy("v2.0", 10)

	rs := store.NewReviewStore(abLogger(), "")
	return vm, rs
}

func seedRecords(rs *store.ReviewStore, versionID string, passed, rejected, manualReview int, overrides int, confidence float64) {
	for i := 0; i < passed; i++ {
		rs.Store(&store.ReviewRecord{
			ReviewResult:      types.ReviewResult{AdID: versionID + "_p_" + string(rune('a'+i)), Decision: types.DecisionPassed, Confidence: confidence},
			StrategyVersionID: versionID,
		})
	}
	for i := 0; i < rejected; i++ {
		rec := &store.ReviewRecord{
			ReviewResult:      types.ReviewResult{AdID: versionID + "_r_" + string(rune('a'+i)), Decision: types.DecisionRejected, Confidence: confidence},
			StrategyVersionID: versionID,
		}
		rs.Store(rec)
		if i < overrides {
			rs.UpdateVerification(rec.AdID, store.VerificationOverride, types.DecisionManualReview, "disagree")
		}
	}
	for i := 0; i < manualReview; i++ {
		rs.Store(&store.ReviewRecord{
			ReviewResult:      types.ReviewResult{AdID: versionID + "_m_" + string(rune('a'+i)), Decision: types.DecisionManualReview, Confidence: confidence},
			StrategyVersionID: versionID,
		})
	}
}

func TestABCompare_InsufficientData(t *testing.T) {
	vm, rs := setupABTest(t)
	seedRecords(rs, "v1.0", 50, 10, 5, 1, 0.85)
	seedRecords(rs, "v2.0", 2, 1, 0, 0, 0.90) // only 3 canary records

	comp, err := Compare(vm, rs, DefaultABConfig(), abLogger())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if comp.Recommendation != ABRecommendContinue {
		t.Errorf("recommendation = %s, want CONTINUE", comp.Recommendation)
	}
	if comp.CanaryStats.Total != 3 {
		t.Errorf("canary total = %d, want 3", comp.CanaryStats.Total)
	}
}

func TestABCompare_CanaryHighFalsePositives(t *testing.T) {
	vm, rs := setupABTest(t)
	seedRecords(rs, "v1.0", 50, 10, 5, 1, 0.85) // active: 1 FP / 65 = 1.5%
	seedRecords(rs, "v2.0", 8, 5, 2, 3, 0.80)    // canary: 3 FP / 15 = 20%

	comp, err := Compare(vm, rs, DefaultABConfig(), abLogger())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if comp.Recommendation != ABRecommendRollback {
		t.Errorf("recommendation = %s, want ROLLBACK (canary FP rate 20%% vs active 1.5%%)", comp.Recommendation)
	}
}

func TestABCompare_CanaryBetter(t *testing.T) {
	vm, rs := setupABTest(t)
	seedRecords(rs, "v1.0", 40, 15, 10, 3, 0.80)  // active: 3 FP / 65, conf=0.80
	seedRecords(rs, "v2.0", 8, 3, 1, 0, 0.90)      // canary: 0 FP / 12, conf=0.90

	comp, err := Compare(vm, rs, DefaultABConfig(), abLogger())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if comp.Recommendation != ABRecommendPromote {
		t.Errorf("recommendation = %s, want PROMOTE (canary better confidence, 0 FP)", comp.Recommendation)
	}
}

func TestABCompare_Inconclusive(t *testing.T) {
	vm, rs := setupABTest(t)
	seedRecords(rs, "v1.0", 40, 10, 5, 1, 0.85)
	seedRecords(rs, "v2.0", 8, 3, 1, 0, 0.80) // canary lower confidence but 0 FP

	comp, err := Compare(vm, rs, DefaultABConfig(), abLogger())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if comp.Recommendation != ABRecommendContinue {
		t.Errorf("recommendation = %s, want CONTINUE (inconclusive)", comp.Recommendation)
	}
}

func TestABCompare_NoCanary(t *testing.T) {
	vm := NewVersionManager(abLogger())
	vm.Create("v1.0")
	vm.Deploy("v1.0", 0)
	vm.Promote("v1.0")
	rs := store.NewReviewStore(abLogger(), "")

	_, err := Compare(vm, rs, DefaultABConfig(), abLogger())
	if err == nil {
		t.Error("expected error when no canary deployed")
	}
}

func TestABCompare_NoActive(t *testing.T) {
	vm := NewVersionManager(abLogger())
	rs := store.NewReviewStore(abLogger(), "")

	_, err := Compare(vm, rs, DefaultABConfig(), abLogger())
	if err == nil {
		t.Error("expected error when no active version")
	}
}

func TestABCompare_UpdatesVersionMetrics(t *testing.T) {
	vm, rs := setupABTest(t)
	seedRecords(rs, "v1.0", 10, 5, 2, 1, 0.85)
	seedRecords(rs, "v2.0", 8, 3, 1, 0, 0.90)

	Compare(vm, rs, DefaultABConfig(), abLogger())

	// After Compare, version metrics should be populated.
	v1, _ := vm.Get("v1.0")
	v2, _ := vm.Get("v2.0")

	if v1.Metrics == nil {
		t.Error("v1.0 metrics should be set after Compare")
	}
	if v2.Metrics == nil {
		t.Error("v2.0 metrics should be set after Compare")
	}
}
