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

// --- Active Learning 测试 ---

func TestTrainingPool_PostReviewHook_BoundaryCaseActiveLearning(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.PostReview(types.ReviewResult{AdID: "ad_boundary", Decision: types.DecisionRejected, Confidence: 0.5},
		"adv", "US", "healthcare", "standard")

	if tp.Len() != 1 {
		t.Fatalf("边界案例（0.5）应被采集，got %d", tp.Len())
	}
	rec := tp.Export()[0]
	if rec.Source != SourceActiveLearning {
		t.Errorf("Source = %q, want %q", rec.Source, SourceActiveLearning)
	}
	if rec.Priority != "high" {
		t.Errorf("Priority = %q, want 'high'", rec.Priority)
	}
}

func TestTrainingPool_PostReviewHook_BoundaryEdges(t *testing.T) {
	tests := []struct {
		name       string
		confidence float64
		wantLen    int
	}{
		{"边界下沿 0.4", 0.4, 1},
		{"边界上沿 0.6", 0.6, 1},
		{"间隙区 0.7 不采集", 0.7, 0},
		{"间隙区 0.3 不采集", 0.3, 0},
		{"间隙区 0.89 不采集", 0.89, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp := NewTrainingPool(testLogger(), "")
			tp.PostReview(types.ReviewResult{AdID: "ad_edge", Decision: types.DecisionRejected, Confidence: tt.confidence},
				"adv", "US", "healthcare", "standard")
			if tp.Len() != tt.wantLen {
				t.Errorf("confidence=%.1f: got %d records, want %d", tt.confidence, tp.Len(), tt.wantLen)
			}
		})
	}
}

func TestTrainingPool_PostReviewHook_ManualReviewBoundary(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.PostReview(types.ReviewResult{AdID: "ad_mr_bd", Decision: types.DecisionManualReview, Confidence: 0.5},
		"adv", "US", "healthcare", "standard")

	if tp.Len() != 0 {
		t.Errorf("MANUAL_REVIEW 即使在边界区间也不应采集，got %d", tp.Len())
	}
}

func TestTrainingPool_PostReviewHook_HighConfidenceNormalPriority(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.PostReview(types.ReviewResult{AdID: "ad_hi", Decision: types.DecisionPassed, Confidence: 0.95},
		"adv", "US", "healthcare", "standard")

	if tp.Len() != 1 {
		t.Fatalf("高置信度应被采集，got %d", tp.Len())
	}
	rec := tp.Export()[0]
	if rec.Priority != "normal" {
		t.Errorf("Priority = %q, want 'normal'", rec.Priority)
	}
	if rec.Source != SourceReview {
		t.Errorf("Source = %q, want %q", rec.Source, SourceReview)
	}
}

func TestTrainingPool_Stats_HighPriorityCount(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.Add(&TrainingRecord{AdID: "ad_1", Source: SourceReview, Region: "US", Priority: "normal"})
	tp.Add(&TrainingRecord{AdID: "ad_2", Source: SourceActiveLearning, Region: "US", Priority: "high"})
	tp.Add(&TrainingRecord{AdID: "ad_3", Source: SourceActiveLearning, Region: "EU", Priority: "high"})

	stats := tp.Stats()
	if stats.HighPriorityCount != 2 {
		t.Errorf("HighPriorityCount = %d, want 2", stats.HighPriorityCount)
	}
	if stats.BySource[SourceActiveLearning] != 2 {
		t.Errorf("BySource[active_learning] = %d, want 2", stats.BySource[SourceActiveLearning])
	}
}

func TestTrainingPool_QueryByPriority(t *testing.T) {
	tp := NewTrainingPool(testLogger(), "")
	tp.Add(&TrainingRecord{AdID: "ad_1", Source: SourceReview, Region: "US", Priority: "normal"})
	tp.Add(&TrainingRecord{AdID: "ad_2", Source: SourceActiveLearning, Region: "US", Priority: "high"})
	tp.Add(&TrainingRecord{AdID: "ad_3", Source: SourceActiveLearning, Region: "EU", Priority: "high"})

	priority := "high"
	results := tp.Query(TrainingFilter{Priority: &priority})
	if len(results) != 2 {
		t.Errorf("expected 2 high-priority records, got %d", len(results))
	}
}
