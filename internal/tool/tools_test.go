package tool

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/agent/mock"
	"github.com/BillBeam/adguard-agent/internal/strategy"
	"github.com/BillBeam/adguard-agent/internal/types"
)

func loadMatrix(t *testing.T) *strategy.StrategyMatrix {
	t.Helper()
	dir, _ := filepath.Abs(filepath.Join("..", "..", "data"))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m, err := strategy.NewStrategyMatrix(
		filepath.Join(dir, "policy_kb.json"),
		filepath.Join(dir, "region_rules.json"),
		filepath.Join(dir, "category_risk.json"),
		logger,
	)
	if err != nil {
		t.Fatalf("loading matrix: %v", err)
	}
	return m
}

// --- ContentAnalyzer tests ---

func TestContentAnalyzer_FallbackDetectsMedicineClaim(t *testing.T) {
	// Use mock LLM that returns error → triggers fallback rule-based.
	client := mock.NewLLMClient()
	client.Errors = []error{&mockErr{}}

	matrix := loadMatrix(t)
	ca := NewContentAnalyzer(client, matrix, testLogger())

	args := json.RawMessage(`{
		"headline": "Miracle Cure for Diabetes - FDA Approved!",
		"body": "100% effective treatment, guaranteed results",
		"category": "healthcare"
	}`)

	result, err := ca.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "unverified_medical_claim") {
		t.Errorf("expected unverified_medical_claim signal, got: %s", result)
	}
	if !strings.Contains(result, "false_regulatory_claim") {
		t.Errorf("expected false_regulatory_claim signal, got: %s", result)
	}
	if !strings.Contains(result, "guaranteed_results_claim") {
		t.Errorf("expected guaranteed_results_claim signal, got: %s", result)
	}
}

func TestContentAnalyzer_FallbackCleanContent(t *testing.T) {
	client := mock.NewLLMClient()
	client.Errors = []error{&mockErr{}}

	matrix := loadMatrix(t)
	ca := NewContentAnalyzer(client, matrix, testLogger())

	args := json.RawMessage(`{
		"headline": "Summer Sale - Up to 60% Off Sneakers",
		"body": "Free shipping on orders over $75",
		"category": "ecommerce"
	}`)

	result, err := ca.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "no_issues_detected") {
		t.Errorf("expected no_issues_detected for clean content, got: %s", result)
	}
}

func TestContentAnalyzer_WithMockLLM(t *testing.T) {
	// Mock LLM returns structured analysis.
	client := mock.NewLLMClient()
	client.Responses = []*types.ChatCompletionResponse{{
		Choices: []types.Choice{{
			Message: types.Message{
				Role: types.RoleAssistant,
				Content: types.NewTextContent(`{"signals":[{"signal":"unverified_medical_claim","severity":"critical","evidence":"claims diabetes cure"}],"signals_count":1}`),
			},
			FinishReason: "stop",
		}},
	}}

	matrix := loadMatrix(t)
	ca := NewContentAnalyzer(client, matrix, testLogger())

	args := json.RawMessage(`{"headline":"Cure Diabetes","body":"miracle treatment","category":"healthcare"}`)
	result, err := ca.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "unverified_medical_claim") {
		t.Errorf("expected LLM-detected signal, got: %s", result)
	}
}

// --- PolicyMatcher tests ---

func TestPolicyMatcher_MENAAlcohol(t *testing.T) {
	matrix := loadMatrix(t)
	pm := NewPolicyMatcher(matrix, testLogger())

	args := json.RawMessage(`{"region":"MENA_SA","category":"alcohol","signals":[]}`)
	result, err := pm.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// MENA_SA + alcohol is prohibited — should get violation even without signals.
	if !strings.Contains(result, "prohibited") {
		t.Errorf("expected prohibition violation for MENA_SA alcohol, got: %s", result)
	}
	if !strings.Contains(result, "critical") {
		t.Errorf("expected critical severity, got: %s", result)
	}
}

func TestPolicyMatcher_USHealthcareWithSignals(t *testing.T) {
	matrix := loadMatrix(t)
	pm := NewPolicyMatcher(matrix, testLogger())

	args := json.RawMessage(`{"region":"US","category":"healthcare","signals":["unverified_medical_claim","false_regulatory_claim"]}`)
	result, err := pm.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "POL_001") || !strings.Contains(result, "POL_002") {
		t.Errorf("expected POL_001 and POL_002 violations, got: %s", truncateStr(result, 300))
	}
}

func TestPolicyMatcher_NoSignalsNoProhibition(t *testing.T) {
	matrix := loadMatrix(t)
	pm := NewPolicyMatcher(matrix, testLogger())

	args := json.RawMessage(`{"region":"US","category":"ecommerce","signals":[]}`)
	result, err := pm.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, `"violations_count":0`) {
		t.Errorf("expected 0 violations for compliant ecommerce, got: %s", result)
	}
}

// --- RegionCompliance tests ---

func TestRegionCompliance_Prohibited(t *testing.T) {
	matrix := loadMatrix(t)
	rc := NewRegionCompliance(matrix, testLogger())

	args := json.RawMessage(`{"region":"MENA_SA","category":"alcohol"}`)
	result, err := rc.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "non_compliant") {
		t.Errorf("expected non_compliant for MENA_SA alcohol, got: %s", result)
	}
	if !strings.Contains(result, "prohibited") {
		t.Errorf("expected prohibited rule_status, got: %s", result)
	}
}

func TestRegionCompliance_Restricted(t *testing.T) {
	matrix := loadMatrix(t)
	rc := NewRegionCompliance(matrix, testLogger())

	args := json.RawMessage(`{"region":"US","category":"healthcare"}`)
	result, err := rc.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "needs_review") {
		t.Errorf("expected needs_review for US healthcare, got: %s", result)
	}
	if !strings.Contains(result, "fda_disclaimer") {
		t.Errorf("expected fda_disclaimer requirement, got: %s", result)
	}
}

func TestRegionCompliance_Permitted(t *testing.T) {
	matrix := loadMatrix(t)
	rc := NewRegionCompliance(matrix, testLogger())

	args := json.RawMessage(`{"region":"US","category":"ecommerce"}`)
	result, err := rc.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "compliant") {
		t.Errorf("expected compliant for US ecommerce, got: %s", result)
	}
}

// --- LandingPageChecker tests ---

func TestLandingPageChecker_NotAccessible(t *testing.T) {
	client := mock.NewLLMClient()
	lpc := NewLandingPageChecker(client, testLogger())

	args := json.RawMessage(`{"url":"https://example.com","description":"page unavailable","is_accessible":false}`)
	result, err := lpc.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "landing_page_not_accessible") {
		t.Errorf("expected accessibility issue, got: %s", result)
	}
}

func TestLandingPageChecker_404(t *testing.T) {
	client := mock.NewLLMClient()
	lpc := NewLandingPageChecker(client, testLogger())

	args := json.RawMessage(`{"url":"https://example.com","description":"HTTP 404 Not Found error"}`)
	result, err := lpc.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "landing_page_404") {
		t.Errorf("expected 404 issue, got: %s", result)
	}
}

func TestLandingPageChecker_SensitiveDataCollection(t *testing.T) {
	client := mock.NewLLMClient()
	lpc := NewLandingPageChecker(client, testLogger())

	args := json.RawMessage(`{"url":"https://example.com","description":"checkout form requesting SSN for verification"}`)
	result, err := lpc.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "excessive_data_collection") {
		t.Errorf("expected data collection issue, got: %s", result)
	}
}

func TestLandingPageChecker_CleanPage(t *testing.T) {
	client := mock.NewLLMClient()
	lpc := NewLandingPageChecker(client, testLogger())

	args := json.RawMessage(`{"url":"https://example.com","description":"Standard product page with privacy policy","is_accessible":true,"is_mobile_optimized":true}`)
	result, err := lpc.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, "no_issues_detected") {
		t.Errorf("expected no issues for clean page, got: %s", result)
	}
}

// --- HistoryLookup tests ---

func TestHistoryLookup_EmptyHistory(t *testing.T) {
	hl := NewHistoryLookup(testLogger())

	args := json.RawMessage(`{"advertiser_id":"adv_001","category":"healthcare","region":"US"}`)
	result, err := hl.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, `"total_history_records":0`) {
		t.Errorf("expected 0 records, got: %s", result)
	}
	if !strings.Contains(result, "unknown") {
		t.Errorf("expected unknown reputation for empty history, got: %s", result)
	}
}

func TestHistoryLookup_WithRecords(t *testing.T) {
	hl := NewHistoryLookup(testLogger())

	// Add 3 records: 1 PASSED, 2 REJECTED — all same advertiser, category, region.
	hl.AddRecord(types.ReviewResult{AdID: "ad_101", Decision: types.DecisionPassed}, "adv_001", "US", "healthcare")
	hl.AddRecord(types.ReviewResult{AdID: "ad_102", Decision: types.DecisionRejected, Violations: []types.PolicyViolation{{PolicyID: "POL_001"}}}, "adv_001", "US", "healthcare")
	hl.AddRecord(types.ReviewResult{AdID: "ad_103", Decision: types.DecisionRejected, Violations: []types.PolicyViolation{{PolicyID: "POL_002"}}}, "adv_001", "US", "healthcare")

	args := json.RawMessage(`{"advertiser_id":"adv_001","category":"healthcare","region":"US"}`)
	result, err := hl.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, `"total_history_records":3`) {
		t.Errorf("expected 3 records, got: %s", result)
	}
	// 1/3 passed = 0.33 → "probation"
	if !strings.Contains(result, "probation") {
		t.Errorf("expected probation reputation, got: %s", truncateStr(result, 300))
	}
}

func TestHistoryLookup_MatchingFilters(t *testing.T) {
	hl := NewHistoryLookup(testLogger())

	// Add records for different advertisers, categories, and regions.
	hl.AddRecord(types.ReviewResult{AdID: "ad_a1", Decision: types.DecisionRejected}, "adv_A", "US", "healthcare")
	hl.AddRecord(types.ReviewResult{AdID: "ad_a2", Decision: types.DecisionPassed}, "adv_A", "US", "healthcare")
	hl.AddRecord(types.ReviewResult{AdID: "ad_b1", Decision: types.DecisionPassed}, "adv_B", "EU", "ecommerce")
	hl.AddRecord(types.ReviewResult{AdID: "ad_b2", Decision: types.DecisionPassed}, "adv_B", "US", "healthcare")

	// Query for adv_A, US, healthcare — should get 2 advertiser records, 3 similar cases (US+healthcare).
	args := json.RawMessage(`{"advertiser_id":"adv_A","category":"healthcare","region":"US"}`)
	result, err := hl.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Advertiser reputation should reflect adv_A's 2 records (1 pass, 1 reject → 50% pass rate).
	if !strings.Contains(result, `"total_reviews":2`) {
		t.Errorf("expected 2 advertiser reviews, got: %s", truncateStr(result, 300))
	}
	if !strings.Contains(result, "flagged") {
		t.Errorf("expected flagged reputation (50%% pass rate), got: %s", truncateStr(result, 300))
	}

	// Similar cases should be 3 (ad_a1, ad_a2, ad_b2 — all US+healthcare).
	if !strings.Contains(result, `"total_history_records":4`) {
		t.Errorf("expected 4 total records, got: %s", truncateStr(result, 300))
	}

	// Query for adv_B — should get different reputation.
	args2 := json.RawMessage(`{"advertiser_id":"adv_B","category":"ecommerce","region":"EU"}`)
	result2, err := hl.Execute(context.Background(), args2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result2, "trusted") {
		t.Errorf("expected trusted reputation for adv_B (100%% pass rate), got: %s", truncateStr(result2, 300))
	}
}

// --- helpers ---

type mockErr struct{}

func (e *mockErr) Error() string { return "mock LLM error" }
