package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/BillBeam/adguard-agent/internal/store"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// HistoryLookup — Perception (感知) + Adjudication (研判) stages
//
// Foundation of the Label-Detect-Train pipeline (标检训一体化) "Detect" stage
// + false-positive control (误伤控制) L1 (Memory consistency).
// Queries historical review precedents for similar ads, providing two layers of
// false-positive control:
//   - Memory consistency: similar ads in the same category/region should receive consistent decisions
//   - Advertiser reputation: high-reputation advertisers get lenient treatment on borderline cases
//
// In the Perception-Attribution-Adjudication-Governance (感知-归因-研判-治理) pipeline:
//   - Perception (感知): retrieves historical precedents, discovers patterns (e.g. an advertiser repeatedly submitting violating content)
//   - Adjudication (研判): uses historical consistency and advertiser reputation to assist decisions
//
// Phase 2 uses in-memory storage; Phase 3 ReviewStore provides persistence.
// HistoryRecord wraps a ReviewResult with the ad context needed for matching.
type HistoryRecord struct {
	types.ReviewResult
	AdvertiserID string `json:"advertiser_id"`
	Region       string `json:"region"`
	Category     string `json:"category"`
}

type HistoryLookup struct {
	BaseTool
	mu          sync.RWMutex
	records     []HistoryRecord
	reviewStore *store.ReviewStore // when non-nil, queries persistent store directly — eliminates data loss on restart
	logger      *slog.Logger
}

// NewHistoryLookup creates a history lookup tool with empty initial history.
func NewHistoryLookup(logger *slog.Logger) *HistoryLookup {
	return &HistoryLookup{
		BaseTool: ReviewToolBase(),
		records:  make([]HistoryRecord, 0, 64),
		logger:   logger,
	}
}

// WithReviewStore associates a persistent store. Once set, Execute queries the
// ReviewStore directly, which automatically includes JSONL-recovered history on
// startup — eliminating history gaps after restarts.
func (h *HistoryLookup) WithReviewStore(rs *store.ReviewStore) *HistoryLookup {
	h.reviewStore = rs
	return h
}

// AddRecord appends a review history entry. No-op when ReviewStore is associated (Store is the single source of truth).
func (h *HistoryLookup) AddRecord(result types.ReviewResult, advertiserID, region, category string) {
	if h.reviewStore != nil {
		return // ReviewStore already holds this record — no separate copy needed
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, HistoryRecord{
		ReviewResult: result,
		AdvertiserID: advertiserID,
		Region:       region,
		Category:     category,
	})
}

func (h *HistoryLookup) Name() string { return "lookup_history" }

func (h *HistoryLookup) Description() string {
	return "Look up historical ad review records for consistency checking and advertiser reputation. " +
		"Returns similar past cases, consistency advice, and advertiser trust score. " +
		"Supports false-positive control: similar ads should receive consistent decisions."
}

func (h *HistoryLookup) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"advertiser_id": {"type": "string", "description": "Advertiser ID to look up history for"},
			"category":      {"type": "string", "description": "Ad category for similar case matching"},
			"region":        {"type": "string", "description": "Region for similar case matching"}
		},
		"required": ["advertiser_id"]
	}`)
}

func (h *HistoryLookup) ValidateInput(args json.RawMessage) error {
	var input struct {
		AdvertiserID string `json:"advertiser_id"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return fmt.Errorf("parsing input: %w", err)
	}
	if input.AdvertiserID == "" {
		return fmt.Errorf("advertiser_id is required")
	}
	return nil
}

// Execute performs Perception (感知) + Adjudication (研判): queries historical precedents and advertiser reputation.
func (h *HistoryLookup) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input struct {
		AdvertiserID string `json:"advertiser_id"`
		Category     string `json:"category"`
		Region       string `json:"region"`
	}
	if isJSONString(args) {
		input.AdvertiserID = unwrapJSONString(args)
	} else if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	// When ReviewStore is associated, query persistent storage directly (includes JSONL-recovered history).
	if h.reviewStore != nil {
		return h.executeFromStore(input.AdvertiserID, input.Category, input.Region)
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	// 1. Find this advertiser's historical records.
	var advertiserRecords []HistoryRecord
	for _, r := range h.records {
		if r.AdvertiserID == input.AdvertiserID {
			advertiserRecords = append(advertiserRecords, r)
		}
	}

	// 2. Find similar cases: same category AND region (any advertiser).
	var similarCases []similarCase
	for _, r := range h.records {
		categoryMatch := input.Category == "" || r.Category == input.Category
		regionMatch := input.Region == "" || r.Region == input.Region
		if categoryMatch && regionMatch {
			similarCases = append(similarCases, similarCase{
				AdID:              r.AdID,
				Decision:          string(r.Decision),
				Confidence:        r.Confidence,
				ViolationsSummary: summarizeViolations(r.Violations),
			})
		}
	}
	// Limit to most recent 10.
	if len(similarCases) > 10 {
		similarCases = similarCases[len(similarCases)-10:]
	}

	// 3. Calculate advertiser reputation from their records.
	rep := calculateReputation(input.AdvertiserID, advertiserRecords)

	// 4. Generate consistency advice.
	advice := generateConsistencyAdvice(similarCases)

	h.logger.Debug("history lookup completed",
		slog.String("advertiser_id", input.AdvertiserID),
		slog.Int("similar_cases", len(similarCases)),
		slog.Int("total_records", len(h.records)),
	)

	result, _ := json.Marshal(map[string]any{
		"similar_cases":          similarCases,
		"consistency_advice":     advice,
		"advertiser_reputation":  rep,
		"total_history_records":  len(h.records),
	})
	return string(result), nil
}

// executeFromStore is the ReviewStore-backed query path, available immediately on startup with JSONL-recovered history.
func (h *HistoryLookup) executeFromStore(advertiserID, category, region string) (string, error) {
	// 1. Advertiser history records.
	advRecords := h.reviewStore.QueryByAdvertiser(advertiserID)
	var histRecords []HistoryRecord
	for _, r := range advRecords {
		histRecords = append(histRecords, HistoryRecord{
			ReviewResult: r.ReviewResult,
			AdvertiserID: r.AdvertiserID,
			Region:       r.Region,
			Category:     r.Category,
		})
	}

	// 2. Similar cases: same category + same region.
	var similarCases []similarCase
	if category != "" && region != "" {
		for _, r := range h.reviewStore.QueryByRegionCategory(region, category) {
			similarCases = append(similarCases, similarCase{
				AdID:              r.AdID,
				Decision:          string(r.Decision),
				Confidence:        r.Confidence,
				ViolationsSummary: summarizeViolations(r.Violations),
			})
		}
	} else {
		// Match by advertiser only.
		for _, r := range advRecords {
			similarCases = append(similarCases, similarCase{
				AdID:              r.AdID,
				Decision:          string(r.Decision),
				Confidence:        r.Confidence,
				ViolationsSummary: summarizeViolations(r.Violations),
			})
		}
	}
	if len(similarCases) > 10 {
		similarCases = similarCases[len(similarCases)-10:]
	}

	// 3. Advertiser reputation.
	rep := calculateReputation(advertiserID, histRecords)

	// 4. Consistency advice.
	advice := generateConsistencyAdvice(similarCases)

	totalRecords := h.reviewStore.Stats().Total

	h.logger.Debug("history lookup (store-backed)",
		slog.String("advertiser_id", advertiserID),
		slog.Int("similar_cases", len(similarCases)),
		slog.Int("total_records", totalRecords),
	)

	result, _ := json.Marshal(map[string]any{
		"similar_cases":         similarCases,
		"consistency_advice":    advice,
		"advertiser_reputation": rep,
		"total_history_records": totalRecords,
	})
	return string(result), nil
}

type similarCase struct {
	AdID              string  `json:"ad_id"`
	Decision          string  `json:"decision"`
	Confidence        float64 `json:"confidence"`
	ViolationsSummary string  `json:"violations_summary"`
}

type advertiserReputation struct {
	AdvertiserID       string  `json:"advertiser_id"`
	TotalReviews       int     `json:"total_reviews"`
	PassRate           float64 `json:"pass_rate"`
	ViolationCount     int     `json:"violation_count"`
	ReputationCategory string  `json:"reputation_category"`
}

func calculateReputation(advertiserID string, records []HistoryRecord) advertiserReputation {
	total := len(records)
	if total == 0 {
		return advertiserReputation{
			AdvertiserID:       advertiserID,
			ReputationCategory: "unknown",
		}
	}

	passed := 0
	violations := 0
	for _, r := range records {
		if r.Decision == types.DecisionPassed {
			passed++
		}
		violations += len(r.Violations)
	}

	passRate := float64(passed) / float64(total)
	return advertiserReputation{
		AdvertiserID:       advertiserID,
		TotalReviews:       total,
		PassRate:           passRate,
		ViolationCount:     violations,
		ReputationCategory: reputationCategory(passRate, violations),
	}
}

func reputationCategory(passRate float64, violationCount int) string {
	switch {
	case violationCount == 0 && passRate >= 0.9:
		return "trusted"
	case passRate >= 0.7:
		return "standard"
	case passRate >= 0.4:
		return "flagged"
	default:
		return "probation"
	}
}

func summarizeViolations(violations []types.PolicyViolation) string {
	if len(violations) == 0 {
		return "no violations"
	}
	ids := make([]string, 0, len(violations))
	for _, v := range violations {
		ids = append(ids, v.PolicyID)
	}
	return fmt.Sprintf("%d violations: %s", len(violations), joinStrings(ids, ", "))
}

func generateConsistencyAdvice(cases []similarCase) string {
	if len(cases) == 0 {
		return "No similar cases found in history. This is a novel case."
	}
	rejected := 0
	passed := 0
	for _, c := range cases {
		switch c.Decision {
		case "REJECTED":
			rejected++
		case "PASSED":
			passed++
		}
	}
	total := len(cases)
	if rejected > passed {
		return fmt.Sprintf("Historical trend: %d/%d similar cases were REJECTED. Recommend consistent strict treatment.", rejected, total)
	}
	if passed > rejected {
		return fmt.Sprintf("Historical trend: %d/%d similar cases were PASSED. Consistent lenient treatment may be appropriate.", passed, total)
	}
	return fmt.Sprintf("Mixed history: %d PASSED, %d REJECTED out of %d cases. Recommend careful review.", passed, rejected, total)
}

func joinStrings(ss []string, sep string) string {
	return strings.Join(ss, sep)
}
