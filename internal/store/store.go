// Package store implements the ReviewStore — the data foundation for the
// "Label-Detect-Train" (标检训) pipeline.
//
// Business alignment:
//   - "标" (Label): Every review result is structurally stored with full context
//   - "检" (Detect): Verification queries rejected records for re-check
//   - "训" (Train): Phase 5 — training data pool fed by review outcomes
//
// Phase 3 uses in-memory storage (sync.RWMutex + map).
// Phase 5 can swap in a persistent backend without changing the interface.
package store

import (
	"log/slog"
	"sync"
	"time"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// VerificationStatus tracks the state of a review record's quality check.
type VerificationStatus string

const (
	VerificationNone      VerificationStatus = ""          // not yet verified
	VerificationPending   VerificationStatus = "pending"   // queued for verification
	VerificationConfirmed VerificationStatus = "confirmed" // LLM-as-Judge agreed with REJECTED
	VerificationOverride  VerificationStatus = "override"  // LLM-as-Judge disagreed → MANUAL_REVIEW
)

// ReviewRecord extends ReviewResult with review context and quality markers.
// This is the primary data entity in the ReviewStore.
type ReviewRecord struct {
	types.ReviewResult

	// Ad context (from AdContent).
	AdvertiserID string `json:"advertiser_id"`
	Region       string `json:"region"`
	Category     string `json:"category"`
	Pipeline     string `json:"pipeline"`

	// Strategy version used for this review.
	StrategyVersionID string `json:"strategy_version_id,omitempty"`

	// Verification markers.
	VerificationStatus VerificationStatus   `json:"verification_status"`
	VerifiedDecision   types.ReviewDecision `json:"verified_decision,omitempty"`
	VerifiedAt         time.Time            `json:"verified_at,omitempty"`
	VerifyReasoning    string               `json:"verify_reasoning,omitempty"`
}

// ReviewStats provides aggregate statistics over stored records.
type ReviewStats struct {
	Total             int                              `json:"total"`
	ByDecision        map[types.ReviewDecision]int     `json:"by_decision"`
	ByRegion          map[string]int                   `json:"by_region"`
	ByPipeline        map[string]int                   `json:"by_pipeline"`
	ByVersion         map[string]int                   `json:"by_version"`
	AverageConfidence float64                          `json:"average_confidence"`
	PassRate          float64                          `json:"pass_rate"`
	VerifiedCount     int                              `json:"verified_count"`
	OverrideCount     int                              `json:"override_count"`
	// FalsePositiveCount tracks verification overrides — reviews where the system
	// was wrong (REJECTED → MANUAL_REVIEW on verification disagree).
	// Used by A/B comparison to detect strategy regression.
	FalsePositiveCount int `json:"false_positive_count"`
}

// ReviewStore manages review records in memory with optional JSONL persistence.
// Thread-safe via sync.RWMutex.
type ReviewStore struct {
	mu           sync.RWMutex
	records      map[string]*ReviewRecord // key = ad_id
	byAdvertiser map[string][]string      // advertiser_id → []ad_id (secondary index)
	jsonl        *JSONLWriter             // nil = no persistence (backward compatible)
	logger       *slog.Logger
}

// NewReviewStore creates a review store. If jsonlPath is non-empty, enables
// JSONL persistence and recovers existing records from the file on startup.
// Pass "" for jsonlPath to use pure in-memory mode (e.g., mock tests).
func NewReviewStore(logger *slog.Logger, jsonlPath string) *ReviewStore {
	rs := &ReviewStore{
		records:      make(map[string]*ReviewRecord),
		byAdvertiser: make(map[string][]string),
		logger:       logger,
	}

	if jsonlPath == "" {
		return rs
	}

	// Recover existing records from JSONL.
	records, skipped, err := ReadJSONL[ReviewRecord](jsonlPath)
	if err != nil {
		logger.Error("failed to read review JSONL", slog.String("path", jsonlPath), slog.String("error", err.Error()))
	}
	for i := range records {
		r := &records[i]
		rs.records[r.AdID] = r
		// Rebuild advertiser index.
		advID := r.AdvertiserID
		rs.byAdvertiser[advID] = append(rs.byAdvertiser[advID], r.AdID)
	}
	if len(records) > 0 || skipped > 0 {
		logger.Info("restored reviews from JSONL",
			slog.String("path", jsonlPath),
			slog.Int("count", len(records)),
			slog.Int("skipped", skipped),
		)
	}

	// Open writer for new records.
	w, err := NewJSONLWriter(jsonlPath, logger)
	if err != nil {
		logger.Error("failed to open review JSONL writer", slog.String("path", jsonlPath), slog.String("error", err.Error()))
		return rs // degrade gracefully: in-memory only
	}
	w.SetCount(len(records))
	rs.jsonl = w

	return rs
}

// Store inserts or updates a review record.
func (rs *ReviewStore) Store(record *ReviewRecord) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	adID := record.AdID
	rs.records[adID] = record

	// Update advertiser index (avoid duplicates).
	advID := record.AdvertiserID
	existing := rs.byAdvertiser[advID]
	for _, id := range existing {
		if id == adID {
			goto persist
		}
	}
	rs.byAdvertiser[advID] = append(existing, adID)

persist:
	if rs.jsonl != nil {
		rs.jsonl.Append(record)
	}
}

// Get retrieves a single record by ad_id.
func (rs *ReviewStore) Get(adID string) (*ReviewRecord, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	r, ok := rs.records[adID]
	return r, ok
}

// QueryByAdvertiser returns all records for a given advertiser.
func (rs *ReviewStore) QueryByAdvertiser(advertiserID string) []*ReviewRecord {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	adIDs := rs.byAdvertiser[advertiserID]
	results := make([]*ReviewRecord, 0, len(adIDs))
	for _, id := range adIDs {
		if r, ok := rs.records[id]; ok {
			results = append(results, r)
		}
	}
	return results
}

// QueryByRegionCategory returns records matching region AND category.
func (rs *ReviewStore) QueryByRegionCategory(region, category string) []*ReviewRecord {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	var results []*ReviewRecord
	for _, r := range rs.records {
		if r.Region == region && r.Category == category {
			results = append(results, r)
		}
	}
	return results
}

// QueryByDecision returns all records with the given decision.
func (rs *ReviewStore) QueryByDecision(decision types.ReviewDecision) []*ReviewRecord {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	var results []*ReviewRecord
	for _, r := range rs.records {
		if r.Decision == decision {
			results = append(results, r)
		}
	}
	return results
}

// QueryUnverifiedRejected returns REJECTED records that haven't been verified.
// Used by the Verification system to find records needing quality check.
func (rs *ReviewStore) QueryUnverifiedRejected() []*ReviewRecord {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	var results []*ReviewRecord
	for _, r := range rs.records {
		if r.Decision == types.DecisionRejected && r.VerificationStatus == VerificationNone {
			results = append(results, r)
		}
	}
	return results
}

// UpdateVerification updates the verification status and decision for a record.
func (rs *ReviewStore) UpdateVerification(adID string, status VerificationStatus, decision types.ReviewDecision, reasoning string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	r, ok := rs.records[adID]
	if !ok {
		return
	}
	r.VerificationStatus = status
	r.VerifiedDecision = decision
	r.VerifiedAt = time.Now()
	r.VerifyReasoning = reasoning

	// Persist the updated record.
	if rs.jsonl != nil {
		rs.jsonl.Append(r)
	}
}

// Flush ensures all JSONL data is written to disk. No-op if persistence is disabled.
func (rs *ReviewStore) Flush() {
	if rs.jsonl != nil {
		rs.jsonl.Flush()
	}
}

// JSONLCount returns the number of persisted records, or 0 if persistence is disabled.
func (rs *ReviewStore) JSONLCount() int {
	if rs.jsonl == nil {
		return 0
	}
	return rs.jsonl.Count()
}

// JSONLPath returns the JSONL file path, or "" if persistence is disabled.
func (rs *ReviewStore) JSONLPath() string {
	if rs.jsonl == nil {
		return ""
	}
	return rs.jsonl.Path()
}

// SetVersionID stamps a record with the strategy version used for review.
func (rs *ReviewStore) SetVersionID(adID, versionID string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if r, ok := rs.records[adID]; ok {
		r.StrategyVersionID = versionID
	}
}

// Stats computes aggregate statistics over all stored records.
func (rs *ReviewStore) Stats() ReviewStats {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	stats := ReviewStats{
		ByDecision: make(map[types.ReviewDecision]int),
		ByRegion:   make(map[string]int),
		ByPipeline: make(map[string]int),
		ByVersion:  make(map[string]int),
	}

	var totalConfidence float64
	for _, r := range rs.records {
		stats.Total++
		stats.ByDecision[r.Decision]++
		stats.ByRegion[r.Region]++
		if r.Pipeline != "" {
			stats.ByPipeline[r.Pipeline]++
		}
		if r.StrategyVersionID != "" {
			stats.ByVersion[r.StrategyVersionID]++
		}
		totalConfidence += r.Confidence

		if r.VerificationStatus == VerificationConfirmed || r.VerificationStatus == VerificationOverride {
			stats.VerifiedCount++
		}
		if r.VerificationStatus == VerificationOverride {
			stats.OverrideCount++
		}
	}

	if stats.Total > 0 {
		stats.AverageConfidence = totalConfidence / float64(stats.Total)
		stats.PassRate = float64(stats.ByDecision[types.DecisionPassed]) / float64(stats.Total)
	}

	return stats
}

// VersionStats computes review statistics for a specific strategy version.
// Filters records to those matching versionID, then computes the same metrics
// as Stats() plus FalsePositiveCount (verification overrides).
// Used by A/B comparison to evaluate canary vs active performance.
func (rs *ReviewStore) VersionStats(versionID string) ReviewStats {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	stats := ReviewStats{
		ByDecision: make(map[types.ReviewDecision]int),
		ByRegion:   make(map[string]int),
		ByPipeline: make(map[string]int),
		ByVersion:  make(map[string]int),
	}

	var totalConfidence float64
	for _, r := range rs.records {
		if r.StrategyVersionID != versionID {
			continue
		}
		stats.Total++
		stats.ByDecision[r.Decision]++
		stats.ByRegion[r.Region]++
		if r.Pipeline != "" {
			stats.ByPipeline[r.Pipeline]++
		}
		stats.ByVersion[r.StrategyVersionID]++
		totalConfidence += r.Confidence

		if r.VerificationStatus == VerificationConfirmed || r.VerificationStatus == VerificationOverride {
			stats.VerifiedCount++
		}
		if r.VerificationStatus == VerificationOverride {
			stats.OverrideCount++
			stats.FalsePositiveCount++
		}
	}

	if stats.Total > 0 {
		stats.AverageConfidence = totalConfidence / float64(stats.Total)
		stats.PassRate = float64(stats.ByDecision[types.DecisionPassed]) / float64(stats.Total)
	}

	return stats
}

// PostReview implements the agent.PostReviewHook interface via Go's implicit
// interface satisfaction. Converts ReviewResult to ReviewRecord and stores it.
//
// This is called by the HookChain after each review completes.
func (rs *ReviewStore) PostReview(result types.ReviewResult, advertiserID, region, category, pipeline string) {
	record := &ReviewRecord{
		ReviewResult: result,
		AdvertiserID: advertiserID,
		Region:       region,
		Category:     category,
		Pipeline:     pipeline,
	}
	rs.Store(record)
	rs.logger.Debug("review stored",
		slog.String("ad_id", result.AdID),
		slog.String("decision", string(result.Decision)),
	)
}
