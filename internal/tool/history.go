package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// HistoryLookup — 感知+研判环节
//
// 业务归属：JD"标检训一体化"的"检"环节基础 + 误伤控制 L1（Memory 一致性）。
// 查询相似广告的历史审核判例，提供两层误伤控制：
//   - Memory 一致性：同一品类/地区的相似广告应得到一致的审核结果
//   - 广告主信誉参考：高信誉广告主在边界案例中倾向宽松判定
//
// 在"感知-归因-研判-治理"链路中：
//   - 感知：检索历史判例，发现模式（如某广告主反复提交违规内容）
//   - 研判：基于历史一致性和广告主信誉辅助判定
//
// Phase 2 实现为内存存储；Phase 3 ReviewStore 将提供持久化。
// HistoryRecord wraps a ReviewResult with the ad context needed for matching.
type HistoryRecord struct {
	types.ReviewResult
	AdvertiserID string `json:"advertiser_id"`
	Region       string `json:"region"`
	Category     string `json:"category"`
}

type HistoryLookup struct {
	BaseTool
	mu      sync.RWMutex
	records []HistoryRecord
	logger  *slog.Logger
}

// NewHistoryLookup creates a HistoryLookup tool with empty history.
func NewHistoryLookup(logger *slog.Logger) *HistoryLookup {
	return &HistoryLookup{
		BaseTool: ReviewToolBase(),
		records:  make([]HistoryRecord, 0, 64),
		logger:   logger,
	}
}

// AddRecord appends a completed review result with ad context to the history.
// Called by Executor.PostReview() after each review completes.
// Thread-safe.
func (h *HistoryLookup) AddRecord(result types.ReviewResult, advertiserID, region, category string) {
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

// Execute — 感知+研判：查询历史判例和广告主信誉。
func (h *HistoryLookup) Execute(_ context.Context, args json.RawMessage) (string, error) {
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
