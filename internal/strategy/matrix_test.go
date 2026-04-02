package strategy

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// testDataDir resolves the path to the data/ directory from the test file location.
func testDataDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "data"))
	if err != nil {
		t.Fatalf("resolving data dir: %v", err)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skipf("data directory not found at %s", dir)
	}
	return dir
}

func loadTestMatrix(t *testing.T) *StrategyMatrix {
	t.Helper()
	dir := testDataDir(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m, err := NewStrategyMatrix(
		filepath.Join(dir, "policy_kb.json"),
		filepath.Join(dir, "region_rules.json"),
		filepath.Join(dir, "category_risk.json"),
		logger,
	)
	if err != nil {
		t.Fatalf("failed to load strategy matrix: %v", err)
	}
	return m
}

// --- Loading tests ---

func TestNewStrategyMatrix_ValidLoad(t *testing.T) {
	m := loadTestMatrix(t)

	if len(m.Policies) == 0 {
		t.Error("expected policies to be loaded")
	}
	if len(m.RegionRules.Rules) == 0 {
		t.Error("expected region rules to be loaded")
	}
	if len(m.CategoryRisk) == 0 {
		t.Error("expected category risk map to be loaded")
	}

	t.Logf("Loaded %d policies, %d regions, %d risk categories",
		len(m.Policies), len(m.RegionRules.Rules), len(m.CategoryRisk))
}

func TestNewStrategyMatrix_InvalidPath(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_, err := NewStrategyMatrix("nonexistent.json", "nonexistent.json", "nonexistent.json", logger)
	if err == nil {
		t.Error("expected error for nonexistent files")
	}
}

// --- GetApplicablePolicies tests ---

func TestGetApplicablePolicies(t *testing.T) {
	m := loadTestMatrix(t)

	tests := []struct {
		name           string
		region         string
		category       string
		wantMinCount   int
		wantSeverity   string // at least one policy must have this severity
		wantSubstring  string // at least one policy's rule_text must contain this
	}{
		{
			name:          "MENA_SA alcohol is totally prohibited",
			region:        "MENA_SA",
			category:      "alcohol",
			wantMinCount:  1,
			wantSeverity:  "critical",
			wantSubstring: "prohibited",
		},
		{
			name:          "US healthcare has restrictions with no_cure_claims",
			region:        "US",
			category:      "healthcare",
			wantMinCount:  1,
			wantSubstring: "cure",
		},
		{
			name:          "gambling is globally prohibited",
			region:        "US",
			category:      "gambling",
			wantMinCount:  1,
			wantSeverity:  "critical",
			wantSubstring: "prohibited",
		},
		{
			name:         "global policies apply via region=Global",
			region:       "SEA_ID",
			category:     "gambling",
			wantMinCount: 1,
			wantSeverity: "critical",
		},
		{
			name:         "MENA prefix matches MENA_SA policies",
			region:       "MENA_SA",
			category:     "all",
			wantMinCount: 1, // POL_019 (MENA_SA, category=all) should match
		},
		{
			name:          "EU prefix matches for EU region policies",
			region:        "EU",
			category:      "healthcare",
			wantMinCount:  1,
			wantSubstring: "CE certification",
		},
		{
			name:         "unknown category returns matching 'all' policies only",
			region:       "US",
			category:     "nonexistent_xyz",
			wantMinCount: 0, // 'all' category policies match, but that's for the category 'all', not an ad category match
		},
		{
			name:          "finance policies include risk disclosure",
			region:        "US",
			category:      "finance",
			wantMinCount:  1,
			wantSubstring: "risk",
		},
		{
			name:          "crypto is prohibited in branded content",
			region:        "Global",
			category:      "crypto",
			wantMinCount:  1,
			wantSubstring: "prohibited",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policies := m.GetApplicablePolicies(tt.region, tt.category)

			if len(policies) < tt.wantMinCount {
				t.Errorf("got %d policies, want at least %d", len(policies), tt.wantMinCount)
				return
			}

			if tt.wantSeverity != "" {
				found := false
				for _, p := range policies {
					if p.Severity == tt.wantSeverity {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected at least one policy with severity %q", tt.wantSeverity)
				}
			}

			if tt.wantSubstring != "" {
				found := false
				for _, p := range policies {
					if containsIgnoreCase(p.RuleText, tt.wantSubstring) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected at least one policy with rule_text containing %q", tt.wantSubstring)
					for _, p := range policies {
						t.Logf("  policy %s: %s", p.ID, truncate(p.RuleText, 80))
					}
				}
			}
		})
	}
}

// --- GetRiskLevel tests ---

func TestGetRiskLevel(t *testing.T) {
	m := loadTestMatrix(t)

	tests := []struct {
		category string
		want     types.RiskLevel
	}{
		{"gambling", types.RiskCritical},
		{"crypto", types.RiskCritical},
		{"weapons", types.RiskCritical},
		{"drugs", types.RiskCritical},
		{"adult_content", types.RiskCritical},
		{"healthcare", types.RiskHigh},
		{"finance", types.RiskHigh},
		{"alcohol", types.RiskHigh},
		{"tobacco", types.RiskHigh},
		{"political", types.RiskHigh},
		{"weight_loss", types.RiskMedium},
		{"supplement", types.RiskMedium},
		{"beauty_cosmetics", types.RiskMedium},
		{"ecommerce", types.RiskLow},
		{"app_promotion", types.RiskLow},
		{"education", types.RiskLow},
		{"unknown_category_xyz", types.RiskMedium}, // fail-safe default
	}

	for _, tt := range tests {
		t.Run(tt.category, func(t *testing.T) {
			got := m.GetRiskLevel(tt.category)
			if got != tt.want {
				t.Errorf("GetRiskLevel(%q) = %q, want %q", tt.category, got, tt.want)
			}
		})
	}
}

// --- GetRegionStrictness tests ---

func TestGetRegionStrictness(t *testing.T) {
	m := loadTestMatrix(t)

	tests := []struct {
		region string
		want   string
	}{
		{"MENA_SA", "strict"},
		{"US", "standard"},
		{"UK", "standard"},
		{"SEA_ID", "standard"},
		{"unknown_region", "strict"}, // fail-closed default
	}

	for _, tt := range tests {
		t.Run(tt.region, func(t *testing.T) {
			got := m.GetRegionStrictness(tt.region)
			if got != tt.want {
				t.Errorf("GetRegionStrictness(%q) = %q, want %q", tt.region, got, tt.want)
			}
		})
	}
}

// --- GetReviewPlan tests ---

func TestGetReviewPlan(t *testing.T) {
	m := loadTestMatrix(t)

	tests := []struct {
		name                string
		region              string
		category            string
		wantPipeline        string
		wantVerification    bool
		wantAllowAutoReject bool
	}{
		{
			name:                "MENA_SA + alcohol → comprehensive (critical risk)",
			region:              "MENA_SA",
			category:            "alcohol",
			wantPipeline:        "comprehensive",
			wantVerification:    true,
			wantAllowAutoReject: true,
		},
		{
			name:                "US + gambling → comprehensive (critical risk)",
			region:              "US",
			category:            "gambling",
			wantPipeline:        "comprehensive",
			wantVerification:    true,
			wantAllowAutoReject: true,
		},
		{
			name:                "US + ecommerce → fast (low risk, standard region)",
			region:              "US",
			category:            "ecommerce",
			wantPipeline:        "fast",
			wantVerification:    false,
			wantAllowAutoReject: false,
		},
		{
			name:                "MENA_SA + ecommerce → comprehensive (strict region)",
			region:              "MENA_SA",
			category:            "ecommerce",
			wantPipeline:        "comprehensive",
			wantVerification:    true,
			wantAllowAutoReject: true,
		},
		{
			name:                "US + healthcare → standard (high risk)",
			region:              "US",
			category:            "healthcare",
			wantPipeline:        "standard",
			wantVerification:    true,
			wantAllowAutoReject: true,
		},
		{
			name:                "US + weight_loss → standard (medium risk)",
			region:              "US",
			category:            "weight_loss",
			wantPipeline:        "standard",
			wantVerification:    false,
			wantAllowAutoReject: true,
		},
		{
			name:                "unknown region → comprehensive (fail-closed strict)",
			region:              "UNKNOWN_XYZ",
			category:            "ecommerce",
			wantPipeline:        "comprehensive",
			wantVerification:    true,
			wantAllowAutoReject: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := m.GetReviewPlan(tt.region, tt.category)

			if plan.Pipeline != tt.wantPipeline {
				t.Errorf("Pipeline = %q, want %q", plan.Pipeline, tt.wantPipeline)
			}
			if plan.RequireVerification != tt.wantVerification {
				t.Errorf("RequireVerification = %v, want %v", plan.RequireVerification, tt.wantVerification)
			}
			if plan.AllowAutoReject != tt.wantAllowAutoReject {
				t.Errorf("AllowAutoReject = %v, want %v", plan.AllowAutoReject, tt.wantAllowAutoReject)
			}
			if plan.MaxTurns <= 0 {
				t.Errorf("MaxTurns should be > 0, got %d", plan.MaxTurns)
			}
			if plan.ConfidenceThreshold <= 0 || plan.ConfidenceThreshold > 1 {
				t.Errorf("ConfidenceThreshold should be (0,1], got %f", plan.ConfidenceThreshold)
			}
			if len(plan.RequiredAgents) == 0 {
				t.Error("RequiredAgents should not be empty")
			}
		})
	}
}

// --- GetRegionCategoryRule tests ---

func TestGetRegionCategoryRule(t *testing.T) {
	m := loadTestMatrix(t)

	tests := []struct {
		name       string
		region     string
		category   string
		wantStatus string
		wantFound  bool
	}{
		{"US alcohol is restricted", "US", "alcohol", "restricted", true},
		{"MENA_SA alcohol is prohibited", "MENA_SA", "alcohol", "prohibited", true},
		{"US ecommerce is permitted", "US", "ecommerce", "permitted", true},
		{"US gambling is prohibited", "US", "gambling", "prohibited", true},
		{"falls back to Global", "UNKNOWN", "gambling", "prohibited", true},
		{"nonexistent category", "US", "nonexistent_xyz", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, found := m.GetRegionCategoryRule(tt.region, tt.category)
			if found != tt.wantFound {
				t.Errorf("found = %v, want %v", found, tt.wantFound)
				return
			}
			if found && rule.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", rule.Status, tt.wantStatus)
			}
		})
	}
}

// --- AdContent parsing test ---

func TestAdContentParsing(t *testing.T) {
	dir := testDataDir(t)
	data, err := os.ReadFile(filepath.Join(dir, "samples", "all_samples.json"))
	if err != nil {
		t.Fatalf("reading all_samples.json: %v", err)
	}

	var samples []types.TestAdSample
	if err := json.Unmarshal(data, &samples); err != nil {
		t.Fatalf("parsing all_samples.json: %v", err)
	}

	if len(samples) == 0 {
		t.Fatal("expected at least one sample")
	}

	t.Logf("Loaded %d test ad samples", len(samples))

	// Verify structure of each sample.
	for _, s := range samples {
		if s.ID == "" {
			t.Error("sample has empty ID")
		}
		if s.Region == "" {
			t.Errorf("sample %s has empty region", s.ID)
		}
		if s.Category == "" {
			t.Errorf("sample %s has empty category", s.ID)
		}
		if s.Content.Headline == "" {
			t.Errorf("sample %s has empty headline", s.ID)
		}
		if s.LandingPage.URL == "" {
			t.Errorf("sample %s has empty landing page URL", s.ID)
		}
		if s.ExpectedResult == "" {
			t.Errorf("sample %s has empty expected_result", s.ID)
		}

		// Validate expected result is one of the known decisions.
		switch s.ExpectedResult {
		case "PASSED", "REJECTED", "MANUAL_REVIEW":
			// ok
		default:
			t.Errorf("sample %s has invalid expected_result %q", s.ID, s.ExpectedResult)
		}
	}
}

// --- Helper functions ---

func containsIgnoreCase(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
