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
}

// ReviewStore manages review records in memory.
// Thread-safe via sync.RWMutex.
type ReviewStore struct {
	mu           sync.RWMutex
	records      map[string]*ReviewRecord // key = ad_id
	byAdvertiser map[string][]string      // advertiser_id → []ad_id (secondary index)
	logger       *slog.Logger
}

// NewReviewStore creates an empty in-memory review store.
func NewReviewStore(logger *slog.Logger) *ReviewStore {
	return &ReviewStore{
		records:      make(map[string]*ReviewRecord),
		byAdvertiser: make(map[string][]string),
		logger:       logger,
	}
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
			return
		}
	}
	rs.byAdvertiser[advID] = append(existing, adID)
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
