package store

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// TrainingPool implements the "训" (Train) stage of the label-detect-train pipeline.
//
// Three data sources feed into the pool:
//   - SourceReview: high-confidence review samples (confidence >= 0.9)
//   - SourceVerificationOverride: Verifier disagreed with REJECTED
//   - SourceAppealOverturn: appeal overturned the original REJECTED
//
// TrainingPool is independent from ReviewStore — it's a downstream consumer
// that receives data via PostReviewHook (for reviews) and explicit Add() calls
// (for verification overrides and appeal overturns).

// TrainingSource identifies the origin of a training record.
type TrainingSource string

const (
	SourceReview              TrainingSource = "review"
	SourceVerificationOverride TrainingSource = "verification_override"
	SourceAppealOverturn      TrainingSource = "appeal_overturn"
)

// TrainingRecord captures one labeled sample for model training.
type TrainingRecord struct {
	RecordID         string              `json:"record_id"`
	AdID             string              `json:"ad_id"`
	AdContent        *types.AdContent    `json:"ad_content,omitempty"`
	OriginalDecision types.ReviewDecision `json:"original_decision"`
	FinalDecision    types.ReviewDecision `json:"final_decision"`
	Source           TrainingSource       `json:"source"`
	Region           string              `json:"region"`
	Category         string              `json:"category"`
	Confidence       float64             `json:"confidence"`
	CreatedAt        time.Time           `json:"created_at"`
}

// TrainingFilter supports composite queries on the training pool.
type TrainingFilter struct {
	Source   *TrainingSource
	Region   *string
	Category *string
}

// TrainingStats provides aggregate statistics.
type TrainingStats struct {
	Total    int                    `json:"total"`
	BySource map[TrainingSource]int `json:"by_source"`
	ByRegion map[string]int         `json:"by_region"`
}

// TrainingPool manages training data records in memory with optional JSONL persistence.
type TrainingPool struct {
	mu      sync.RWMutex
	records []*TrainingRecord
	seen    map[string]bool // "adID:source" dedup key
	jsonl   *JSONLWriter    // nil = no persistence
	logger  *slog.Logger
}

// NewTrainingPool creates a training pool. If jsonlPath is non-empty, enables
// JSONL persistence and recovers existing records on startup.
func NewTrainingPool(logger *slog.Logger, jsonlPath string) *TrainingPool {
	tp := &TrainingPool{
		records: make([]*TrainingRecord, 0, 64),
		seen:    make(map[string]bool),
		logger:  logger,
	}

	if jsonlPath == "" {
		return tp
	}

	records, skipped, err := ReadJSONL[TrainingRecord](jsonlPath)
	if err != nil {
		logger.Error("failed to read training JSONL", slog.String("error", err.Error()))
	}
	for i := range records {
		r := &records[i]
		key := fmt.Sprintf("%s:%s", r.AdID, r.Source)
		if tp.seen[key] {
			continue
		}
		tp.seen[key] = true
		tp.records = append(tp.records, r)
	}
	if len(records) > 0 || skipped > 0 {
		logger.Info("restored training records from JSONL",
			slog.Int("count", len(tp.records)), slog.Int("skipped", skipped))
	}

	w, err := NewJSONLWriter(jsonlPath, logger)
	if err != nil {
		logger.Error("failed to open training JSONL writer", slog.String("error", err.Error()))
		return tp
	}
	w.SetCount(len(tp.records))
	tp.jsonl = w
	return tp
}

// Add inserts a training record. Deduplicates by adID+source.
func (tp *TrainingPool) Add(record *TrainingRecord) {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	key := fmt.Sprintf("%s:%s", record.AdID, record.Source)
	if tp.seen[key] {
		return
	}
	if record.RecordID == "" {
		record.RecordID = fmt.Sprintf("tr_%s_%s", record.AdID, record.Source)
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	tp.seen[key] = true
	tp.records = append(tp.records, record)

	if tp.jsonl != nil {
		tp.jsonl.Append(record)
	}

	tp.logger.Debug("training record added",
		slog.String("ad_id", record.AdID),
		slog.String("source", string(record.Source)),
	)
}

// Flush ensures all JSONL data is written to disk. No-op if persistence is disabled.
func (tp *TrainingPool) Flush() {
	if tp.jsonl != nil {
		tp.jsonl.Flush()
	}
}

// Query returns records matching the filter.
func (tp *TrainingPool) Query(filter TrainingFilter) []*TrainingRecord {
	tp.mu.RLock()
	defer tp.mu.RUnlock()

	var results []*TrainingRecord
	for _, r := range tp.records {
		if filter.Source != nil && r.Source != *filter.Source {
			continue
		}
		if filter.Region != nil && r.Region != *filter.Region {
			continue
		}
		if filter.Category != nil && r.Category != *filter.Category {
			continue
		}
		results = append(results, r)
	}
	return results
}

// Export returns all records.
func (tp *TrainingPool) Export() []*TrainingRecord {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	cp := make([]*TrainingRecord, len(tp.records))
	copy(cp, tp.records)
	return cp
}

// Stats returns aggregate statistics.
func (tp *TrainingPool) Stats() TrainingStats {
	tp.mu.RLock()
	defer tp.mu.RUnlock()

	stats := TrainingStats{
		BySource: make(map[TrainingSource]int),
		ByRegion: make(map[string]int),
	}
	for _, r := range tp.records {
		stats.Total++
		stats.BySource[r.Source]++
		stats.ByRegion[r.Region]++
	}
	return stats
}

// Len returns the number of records.
func (tp *TrainingPool) Len() int {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	return len(tp.records)
}

// PostReview implements agent.PostReviewHook.
// Samples high-confidence reviews (confidence >= 0.9) as training data.
// MANUAL_REVIEW results are excluded (ambiguous labels are not useful for training).
func (tp *TrainingPool) PostReview(result types.ReviewResult, _, region, category, _ string) {
	if result.Confidence < 0.9 {
		return
	}
	if result.Decision == types.DecisionManualReview {
		return
	}
	tp.Add(&TrainingRecord{
		AdID:             result.AdID,
		OriginalDecision: result.Decision,
		FinalDecision:    result.Decision,
		Source:           SourceReview,
		Region:           region,
		Category:         category,
		Confidence:       result.Confidence,
	})
}
