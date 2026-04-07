package store

import (
	"testing"

	"github.com/BillBeam/adguard-agent/internal/types"
)

func TestTrainingPool_Add(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.Add(&TrainingRecord{AdID: "ad_001", Source: SourceReview, Region: "US"})
	if tp.Len() != 1 {
		t.Errorf("expected 1 record, got %d", tp.Len())
	}
}

func TestTrainingPool_DeduplicateByAdIDAndSource(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.Add(&TrainingRecord{AdID: "ad_001", Source: SourceReview})
	tp.Add(&TrainingRecord{AdID: "ad_001", Source: SourceReview}) // duplicate
	tp.Add(&TrainingRecord{AdID: "ad_001", Source: SourceAppealOverturn}) // different source

	if tp.Len() != 2 {
		t.Errorf("expected 2 (dedup same source), got %d", tp.Len())
	}
}

func TestTrainingPool_QueryBySource(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.Add(&TrainingRecord{AdID: "ad_001", Source: SourceReview, Region: "US"})
	tp.Add(&TrainingRecord{AdID: "ad_002", Source: SourceVerificationOverride, Region: "EU"})
	tp.Add(&TrainingRecord{AdID: "ad_003", Source: SourceReview, Region: "US"})

	src := SourceReview
	results := tp.Query(TrainingFilter{Source: &src})
	if len(results) != 2 {
		t.Errorf("expected 2 review records, got %d", len(results))
	}
}

func TestTrainingPool_QueryByRegion(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.Add(&TrainingRecord{AdID: "ad_001", Source: SourceReview, Region: "US"})
	tp.Add(&TrainingRecord{AdID: "ad_002", Source: SourceReview, Region: "EU"})

	region := "US"
	results := tp.Query(TrainingFilter{Region: &region})
	if len(results) != 1 {
		t.Errorf("expected 1 US record, got %d", len(results))
	}
}

func TestTrainingPool_CompositeFilter(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.Add(&TrainingRecord{AdID: "ad_001", Source: SourceReview, Region: "US", Category: "healthcare"})
	tp.Add(&TrainingRecord{AdID: "ad_002", Source: SourceReview, Region: "US", Category: "ecommerce"})
	tp.Add(&TrainingRecord{AdID: "ad_003", Source: SourceAppealOverturn, Region: "US", Category: "healthcare"})

	src := SourceReview
	cat := "healthcare"
	results := tp.Query(TrainingFilter{Source: &src, Category: &cat})
	if len(results) != 1 {
		t.Errorf("expected 1 review+healthcare, got %d", len(results))
	}
}

func TestTrainingPool_Export(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.Add(&TrainingRecord{AdID: "ad_001", Source: SourceReview})
	tp.Add(&TrainingRecord{AdID: "ad_002", Source: SourceAppealOverturn})

	exported := tp.Export()
	if len(exported) != 2 {
		t.Errorf("expected 2 exported, got %d", len(exported))
	}
}

func TestTrainingPool_Stats(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.Add(&TrainingRecord{AdID: "ad_001", Source: SourceReview, Region: "US"})
	tp.Add(&TrainingRecord{AdID: "ad_002", Source: SourceVerificationOverride, Region: "EU"})
	tp.Add(&TrainingRecord{AdID: "ad_003", Source: SourceAppealOverturn, Region: "US"})

	stats := tp.Stats()
	if stats.Total != 3 {
		t.Errorf("expected total=3, got %d", stats.Total)
	}
	if stats.BySource[SourceReview] != 1 {
		t.Errorf("expected 1 review, got %d", stats.BySource[SourceReview])
	}
	if stats.ByRegion["US"] != 2 {
		t.Errorf("expected 2 US, got %d", stats.ByRegion["US"])
	}
}

func TestTrainingPool_PostReviewHook_SamplesHighConfidence(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.PostReview(types.ReviewResult{AdID: "ad_high", Decision: types.DecisionRejected, Confidence: 0.95},
		"adv", "US", "healthcare", "standard")

	if tp.Len() != 1 {
		t.Errorf("high confidence should be sampled, got %d", tp.Len())
	}
}

func TestTrainingPool_PostReviewHook_SkipsLowConfidence(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.PostReview(types.ReviewResult{AdID: "ad_low", Decision: types.DecisionRejected, Confidence: 0.7},
		"adv", "US", "healthcare", "standard")

	if tp.Len() != 0 {
		t.Errorf("low confidence should be skipped, got %d", tp.Len())
	}
}

func TestTrainingPool_PostReviewHook_SkipsManualReview(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.PostReview(types.ReviewResult{AdID: "ad_mr", Decision: types.DecisionManualReview, Confidence: 0.95},
		"adv", "US", "healthcare", "standard")

	if tp.Len() != 0 {
		t.Errorf("MANUAL_REVIEW should be skipped, got %d", tp.Len())
	}
}
