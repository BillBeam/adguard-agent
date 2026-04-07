package store

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// Appeal workflow — "治理"环节的核心：广告主权利救济。
//
// Appeal vs Verification:
//   - Verification: system-triggered quality check (automatic)
//   - Appeal: advertiser-triggered rights remedy (manual trigger)
//
// Appeal lifecycle: SUBMITTED → REVIEWING → RESOLVED
// Appeal outcome: UPHELD (maintain) / OVERTURNED (reverse to PASSED) / PARTIAL

// AppealStatus tracks the appeal lifecycle.
type AppealStatus string

const (
	AppealSubmitted AppealStatus = "SUBMITTED"
	AppealReviewing AppealStatus = "REVIEWING"
	AppealResolved  AppealStatus = "RESOLVED"
)

// AppealOutcome describes how the appeal was resolved.
type AppealOutcome string

const (
	AppealUpheld    AppealOutcome = "UPHELD"
	AppealOverturned AppealOutcome = "OVERTURNED"
	AppealPartial   AppealOutcome = "PARTIAL"
)

// Appeal represents an advertiser's appeal against a REJECTED review.
type Appeal struct {
	AppealID     string        `json:"appeal_id"`
	AdID         string        `json:"ad_id"`
	AdvertiserID string        `json:"advertiser_id"`
	Reason       string        `json:"reason"`
	Status       AppealStatus  `json:"status"`
	Outcome      AppealOutcome `json:"outcome,omitempty"`
	Reasoning    string        `json:"reasoning,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	ResolvedAt   time.Time     `json:"resolved_at,omitempty"`
}

// AppealStats provides aggregate statistics.
type AppealStats struct {
	Total     int                       `json:"total"`
	ByOutcome map[AppealOutcome]int     `json:"by_outcome"`
	ByStatus  map[AppealStatus]int      `json:"by_status"`
}

// AppealStore manages appeal records in memory with optional JSONL persistence. Thread-safe.
type AppealStore struct {
	mu          sync.RWMutex
	appeals     map[string]*Appeal // key = appeal_id
	byAdID      map[string]string  // ad_id → appeal_id (one-per-ad)
	reputation  *ReputationManager // linked for auto-update on resolve
	jsonl       *JSONLWriter       // nil = no persistence
	logger      *slog.Logger
}

// NewAppealStore creates an appeal store. If jsonlPath is non-empty, enables
// JSONL persistence and recovers existing appeals from the file on startup.
func NewAppealStore(logger *slog.Logger, rm *ReputationManager, jsonlPath string) *AppealStore {
	as := &AppealStore{
		appeals:    make(map[string]*Appeal),
		byAdID:     make(map[string]string),
		reputation: rm,
		logger:     logger,
	}

	if jsonlPath == "" {
		return as
	}

	// Recover existing appeals from JSONL.
	appeals, skipped, err := ReadJSONL[Appeal](jsonlPath)
	if err != nil {
		logger.Error("failed to read appeal JSONL", slog.String("error", err.Error()))
	}
	for i := range appeals {
		a := &appeals[i]
		as.appeals[a.AppealID] = a
		as.byAdID[a.AdID] = a.AppealID
	}
	if len(appeals) > 0 || skipped > 0 {
		logger.Info("restored appeals from JSONL",
			slog.Int("count", len(appeals)), slog.Int("skipped", skipped))
	}

	w, err := NewJSONLWriter(jsonlPath, logger)
	if err != nil {
		logger.Error("failed to open appeal JSONL writer", slog.String("error", err.Error()))
		return as
	}
	w.SetCount(len(appeals))
	as.jsonl = w
	return as
}

// Submit creates a new appeal. Returns error if the ad already has an appeal.
func (as *AppealStore) Submit(adID, advertiserID, reason string) (*Appeal, error) {
	as.mu.Lock()
	defer as.mu.Unlock()

	if _, exists := as.byAdID[adID]; exists {
		return nil, fmt.Errorf("appeal already exists for ad %s (one appeal per ad)", adID)
	}

	appeal := &Appeal{
		AppealID:     generateAppealID(),
		AdID:         adID,
		AdvertiserID: advertiserID,
		Reason:       reason,
		Status:       AppealSubmitted,
		CreatedAt:    time.Now(),
	}

	as.appeals[appeal.AppealID] = appeal
	as.byAdID[adID] = appeal.AppealID

	if as.jsonl != nil {
		as.jsonl.Append(appeal)
	}

	as.logger.Info("appeal submitted",
		slog.String("appeal_id", appeal.AppealID),
		slog.String("ad_id", adID),
	)
	return appeal, nil
}

// Get returns an appeal by ID.
func (as *AppealStore) Get(appealID string) (*Appeal, bool) {
	as.mu.RLock()
	defer as.mu.RUnlock()
	a, ok := as.appeals[appealID]
	return a, ok
}

// GetByAdID returns the appeal for a given ad.
func (as *AppealStore) GetByAdID(adID string) (*Appeal, bool) {
	as.mu.RLock()
	defer as.mu.RUnlock()
	appealID, ok := as.byAdID[adID]
	if !ok {
		return nil, false
	}
	return as.appeals[appealID], true
}

// SetReviewing transitions an appeal to REVIEWING status.
func (as *AppealStore) SetReviewing(appealID string) error {
	as.mu.Lock()
	defer as.mu.Unlock()
	a, ok := as.appeals[appealID]
	if !ok {
		return fmt.Errorf("appeal %q not found", appealID)
	}
	if a.Status != AppealSubmitted {
		return fmt.Errorf("appeal %q is %s, must be SUBMITTED", appealID, a.Status)
	}
	a.Status = AppealReviewing
	return nil
}

// Resolve completes an appeal with the given outcome.
// Automatically updates advertiser reputation via ReputationManager.
func (as *AppealStore) Resolve(appealID string, outcome AppealOutcome, reasoning string) error {
	as.mu.Lock()
	defer as.mu.Unlock()

	a, ok := as.appeals[appealID]
	if !ok {
		return fmt.Errorf("appeal %q not found", appealID)
	}
	if a.Status == AppealResolved {
		return fmt.Errorf("appeal %q already resolved", appealID)
	}

	a.Status = AppealResolved
	a.Outcome = outcome
	a.Reasoning = reasoning
	a.ResolvedAt = time.Now()

	if as.jsonl != nil {
		as.jsonl.Append(a)
	}

	as.logger.Info("appeal resolved",
		slog.String("appeal_id", appealID),
		slog.String("outcome", string(outcome)),
	)

	// Update advertiser reputation based on outcome.
	if as.reputation != nil {
		as.reputation.UpdateOnAppeal(a.AdvertiserID, outcome)
	}

	return nil
}

// Flush ensures all JSONL data is written to disk. No-op if persistence is disabled.
func (as *AppealStore) Flush() {
	if as.jsonl != nil {
		as.jsonl.Flush()
	}
}

// QueryByAdvertiser returns all appeals for a given advertiser.
func (as *AppealStore) QueryByAdvertiser(advertiserID string) []*Appeal {
	as.mu.RLock()
	defer as.mu.RUnlock()
	var results []*Appeal
	for _, a := range as.appeals {
		if a.AdvertiserID == advertiserID {
			results = append(results, a)
		}
	}
	return results
}

// Stats returns aggregate statistics.
func (as *AppealStore) Stats() AppealStats {
	as.mu.RLock()
	defer as.mu.RUnlock()
	stats := AppealStats{
		ByOutcome: make(map[AppealOutcome]int),
		ByStatus:  make(map[AppealStatus]int),
	}
	for _, a := range as.appeals {
		stats.Total++
		stats.ByStatus[a.Status]++
		if a.Outcome != "" {
			stats.ByOutcome[a.Outcome]++
		}
	}
	return stats
}

func generateAppealID() string {
	var b [8]byte
	rand.Read(b[:])
	return fmt.Sprintf("appeal_%x", b)
}

// --- ReputationManager ---

// ReputationManager tracks advertiser trust scores and appeal outcomes.
// Uses types.AdvertiserReputation which was defined in Phase 0 but unused until now.
type ReputationManager struct {
	mu          sync.RWMutex
	reputations map[string]*types.AdvertiserReputation
	logger      *slog.Logger
}

// NewReputationManager creates an empty reputation manager.
func NewReputationManager(logger *slog.Logger) *ReputationManager {
	return &ReputationManager{
		reputations: make(map[string]*types.AdvertiserReputation),
		logger:      logger,
	}
}

// Get returns the reputation for an advertiser, creating a default if needed.
func (rm *ReputationManager) Get(advertiserID string) *types.AdvertiserReputation {
	rm.mu.RLock()
	if rep, ok := rm.reputations[advertiserID]; ok {
		rm.mu.RUnlock()
		return rep
	}
	rm.mu.RUnlock()

	rm.mu.Lock()
	defer rm.mu.Unlock()
	// Double-check after acquiring write lock.
	if rep, ok := rm.reputations[advertiserID]; ok {
		return rep
	}
	rep := &types.AdvertiserReputation{
		AdvertiserID: advertiserID,
		TrustScore:   0.5,
		RiskCategory: "standard",
	}
	rm.reputations[advertiserID] = rep
	return rep
}

// UpdateOnAppeal adjusts reputation based on appeal outcome.
//   - OVERTURNED: system was wrong → trust up, AppealSuccessRate up
//   - UPHELD: advertiser pushed bad content → trust down, HistoricalViolations up
func (rm *ReputationManager) UpdateOnAppeal(advertiserID string, outcome AppealOutcome) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	rep, ok := rm.reputations[advertiserID]
	if !ok {
		rep = &types.AdvertiserReputation{
			AdvertiserID: advertiserID,
			TrustScore:   0.5,
			RiskCategory: "standard",
		}
		rm.reputations[advertiserID] = rep
	}

	switch outcome {
	case AppealOverturned:
		rep.TrustScore += 0.1
		if rep.TrustScore > 1.0 {
			rep.TrustScore = 1.0
		}
		// Update success rate: (old_rate * old_count + 1) / (old_count + 1)
		totalAppeals := rep.HistoricalViolations + 1 // rough approximation
		rep.AppealSuccessRate = (rep.AppealSuccessRate*float64(totalAppeals-1) + 1.0) / float64(totalAppeals)

	case AppealUpheld:
		rep.TrustScore -= 0.1
		if rep.TrustScore < 0.0 {
			rep.TrustScore = 0.0
		}
		rep.HistoricalViolations++
		totalAppeals := rep.HistoricalViolations
		rep.AppealSuccessRate = rep.AppealSuccessRate * float64(totalAppeals-1) / float64(totalAppeals)
	}

	// Update risk category based on trust score.
	rep.RiskCategory = classifyRisk(rep.TrustScore)

	rm.logger.Debug("reputation updated",
		slog.String("advertiser", advertiserID),
		slog.Float64("trust_score", rep.TrustScore),
		slog.String("risk_category", rep.RiskCategory),
	)
}

// RecordViolation increments the violation count for an advertiser.
func (rm *ReputationManager) RecordViolation(advertiserID string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rep := rm.reputations[advertiserID]
	if rep == nil {
		rep = &types.AdvertiserReputation{
			AdvertiserID: advertiserID,
			TrustScore:   0.5,
			RiskCategory: "standard",
		}
		rm.reputations[advertiserID] = rep
	}
	rep.HistoricalViolations++
	rep.TrustScore -= 0.05
	if rep.TrustScore < 0 {
		rep.TrustScore = 0
	}
	rep.RiskCategory = classifyRisk(rep.TrustScore)
}

func classifyRisk(trustScore float64) string {
	switch {
	case trustScore >= 0.8:
		return "trusted"
	case trustScore >= 0.5:
		return "standard"
	case trustScore >= 0.3:
		return "flagged"
	default:
		return "probation"
	}
}
