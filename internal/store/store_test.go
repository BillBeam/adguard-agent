package store

import (
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/types"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestStore_StoreAndGet(t *testing.T) {
	rs := NewReviewStore(testLogger(), "")
	record := &ReviewRecord{
		ReviewResult: types.ReviewResult{AdID: "ad_001", Decision: types.DecisionPassed, Confidence: 0.95},
		AdvertiserID: "adv_001",
		Region:       "US",
		Category:     "ecommerce",
	}
	rs.Store(record)

	got, ok := rs.Get("ad_001")
	if !ok {
		t.Fatal("expected record to exist")
	}
	if got.Decision != types.DecisionPassed || got.Confidence != 0.95 {
		t.Errorf("got decision=%s confidence=%.2f", got.Decision, got.Confidence)
	}

	// Non-existent record.
	_, ok = rs.Get("ad_999")
	if ok {
		t.Error("expected non-existent record to return false")
	}
}

func TestStore_QueryByAdvertiser(t *testing.T) {
	rs := NewReviewStore(testLogger(), "")
	rs.Store(&ReviewRecord{ReviewResult: types.ReviewResult{AdID: "ad_001"}, AdvertiserID: "adv_A"})
	rs.Store(&ReviewRecord{ReviewResult: types.ReviewResult{AdID: "ad_002"}, AdvertiserID: "adv_A"})
	rs.Store(&ReviewRecord{ReviewResult: types.ReviewResult{AdID: "ad_003"}, AdvertiserID: "adv_B"})

	results := rs.QueryByAdvertiser("adv_A")
	if len(results) != 2 {
		t.Errorf("expected 2 records for adv_A, got %d", len(results))
	}

	results = rs.QueryByAdvertiser("adv_B")
	if len(results) != 1 {
		t.Errorf("expected 1 record for adv_B, got %d", len(results))
	}

	results = rs.QueryByAdvertiser("adv_C")
	if len(results) != 0 {
		t.Errorf("expected 0 records for adv_C, got %d", len(results))
	}
}

func TestStore_QueryByRegionCategory(t *testing.T) {
	rs := NewReviewStore(testLogger(), "")
	rs.Store(&ReviewRecord{ReviewResult: types.ReviewResult{AdID: "ad_001"}, Region: "US", Category: "healthcare"})
	rs.Store(&ReviewRecord{ReviewResult: types.ReviewResult{AdID: "ad_002"}, Region: "US", Category: "healthcare"})
	rs.Store(&ReviewRecord{ReviewResult: types.ReviewResult{AdID: "ad_003"}, Region: "EU", Category: "healthcare"})

	results := rs.QueryByRegionCategory("US", "healthcare")
	if len(results) != 2 {
		t.Errorf("expected 2 US healthcare records, got %d", len(results))
	}

	results = rs.QueryByRegionCategory("EU", "healthcare")
	if len(results) != 1 {
		t.Errorf("expected 1 EU healthcare record, got %d", len(results))
	}
}

func TestStore_QueryUnverifiedRejected(t *testing.T) {
	rs := NewReviewStore(testLogger(), "")
	rs.Store(&ReviewRecord{ReviewResult: types.ReviewResult{AdID: "ad_001", Decision: types.DecisionRejected}})
	rs.Store(&ReviewRecord{ReviewResult: types.ReviewResult{AdID: "ad_002", Decision: types.DecisionRejected}, VerificationStatus: VerificationConfirmed})
	rs.Store(&ReviewRecord{ReviewResult: types.ReviewResult{AdID: "ad_003", Decision: types.DecisionPassed}})
	rs.Store(&ReviewRecord{ReviewResult: types.ReviewResult{AdID: "ad_004", Decision: types.DecisionRejected}})

	results := rs.QueryUnverifiedRejected()
	if len(results) != 2 {
		t.Errorf("expected 2 unverified rejected, got %d", len(results))
	}
}

func TestStore_Stats(t *testing.T) {
	rs := NewReviewStore(testLogger(), "")
	rs.Store(&ReviewRecord{
		ReviewResult: types.ReviewResult{AdID: "ad_001", Decision: types.DecisionPassed, Confidence: 0.90},
		Region:       "US",
	})
	rs.Store(&ReviewRecord{
		ReviewResult:       types.ReviewResult{AdID: "ad_002", Decision: types.DecisionRejected, Confidence: 0.85},
		Region:             "EU",
		VerificationStatus: VerificationConfirmed,
	})
	rs.Store(&ReviewRecord{
		ReviewResult:       types.ReviewResult{AdID: "ad_003", Decision: types.DecisionRejected, Confidence: 0.70},
		Region:             "US",
		VerificationStatus: VerificationOverride,
	})

	stats := rs.Stats()
	if stats.Total != 3 {
		t.Errorf("expected total=3, got %d", stats.Total)
	}
	if stats.ByDecision[types.DecisionPassed] != 1 {
		t.Errorf("expected 1 passed, got %d", stats.ByDecision[types.DecisionPassed])
	}
	if stats.ByDecision[types.DecisionRejected] != 2 {
		t.Errorf("expected 2 rejected, got %d", stats.ByDecision[types.DecisionRejected])
	}
	if stats.VerifiedCount != 2 {
		t.Errorf("expected 2 verified, got %d", stats.VerifiedCount)
	}
	if stats.OverrideCount != 1 {
		t.Errorf("expected 1 override, got %d", stats.OverrideCount)
	}

	// Average confidence: (0.90 + 0.85 + 0.70) / 3 ≈ 0.8167
	if stats.AverageConfidence < 0.81 || stats.AverageConfidence > 0.82 {
		t.Errorf("expected avg confidence ~0.82, got %.4f", stats.AverageConfidence)
	}
	// Pass rate: 1/3 ≈ 0.333
	if stats.PassRate < 0.33 || stats.PassRate > 0.34 {
		t.Errorf("expected pass rate ~0.33, got %.4f", stats.PassRate)
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	rs := NewReviewStore(testLogger(), "")

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			adID := "ad_" + string(rune('A'+idx%26))
			rs.Store(&ReviewRecord{
				ReviewResult: types.ReviewResult{AdID: adID, Decision: types.DecisionPassed},
				AdvertiserID: "adv_001",
			})
			rs.Get(adID)
			rs.Stats()
		}(i)
	}
	wg.Wait()

	// Should not panic, deadlock, or corrupt data.
	stats := rs.Stats()
	if stats.Total == 0 {
		t.Error("expected non-zero total after concurrent writes")
	}
}

func TestStore_PostReviewHook(t *testing.T) {
	rs := NewReviewStore(testLogger(), "")

	result := types.ReviewResult{AdID: "ad_hook", Decision: types.DecisionRejected, Confidence: 0.88}
	rs.PostReview(result, "adv_X", "MENA_SA", "alcohol", "comprehensive")

	got, ok := rs.Get("ad_hook")
	if !ok {
		t.Fatal("PostReview should have stored the record")
	}
	if got.AdvertiserID != "adv_X" || got.Region != "MENA_SA" || got.Category != "alcohol" || got.Pipeline != "comprehensive" {
		t.Errorf("context mismatch: adv=%s region=%s cat=%s pipeline=%s", got.AdvertiserID, got.Region, got.Category, got.Pipeline)
	}
}

func TestStore_SetVersionID_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/reviews.jsonl"

	rs := NewReviewStore(testLogger(), path)
	rs.Store(&ReviewRecord{
		ReviewResult: types.ReviewResult{AdID: "ad_v1", Decision: types.DecisionPassed, Confidence: 0.95},
		AdvertiserID: "adv_001",
		Region:       "US",
	})

	rs.SetVersionID("ad_v1", "v1.0")

	// Verify in-memory update.
	got, ok := rs.Get("ad_v1")
	if !ok {
		t.Fatal("expected record to exist")
	}
	if got.StrategyVersionID != "v1.0" {
		t.Errorf("in-memory version = %q, want %q", got.StrategyVersionID, "v1.0")
	}

	// Verify persistence: recover from JSONL.
	rs.Flush()
	rs2 := NewReviewStore(testLogger(), path)
	got2, ok := rs2.Get("ad_v1")
	if !ok {
		t.Fatal("expected record to survive JSONL recovery")
	}
	if got2.StrategyVersionID != "v1.0" {
		t.Errorf("recovered version = %q, want %q", got2.StrategyVersionID, "v1.0")
	}
}

func TestStore_UpdateVerification(t *testing.T) {
	rs := NewReviewStore(testLogger(), "")
	rs.Store(&ReviewRecord{
		ReviewResult: types.ReviewResult{AdID: "ad_v1", Decision: types.DecisionRejected},
	})

	rs.UpdateVerification("ad_v1", VerificationOverride, types.DecisionManualReview, "violation unclear")

	got, _ := rs.Get("ad_v1")
	if got.VerificationStatus != VerificationOverride {
		t.Errorf("expected override status, got %s", got.VerificationStatus)
	}
	if got.VerifiedDecision != types.DecisionManualReview {
		t.Errorf("expected MANUAL_REVIEW, got %s", got.VerifiedDecision)
	}
	if got.VerifyReasoning != "violation unclear" {
		t.Errorf("expected reasoning, got %s", got.VerifyReasoning)
	}
	if got.VerifiedAt.IsZero() {
		t.Error("expected non-zero verified_at")
	}
}
