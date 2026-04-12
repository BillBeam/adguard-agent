package agent

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

func loadTestMatrix(t *testing.T) *strategy.StrategyMatrix {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "data"))
	if err != nil {
		t.Fatalf("resolving data dir: %v", err)
	}
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

func TestReviewEngine_PipelineSelection(t *testing.T) {
	matrix := loadTestMatrix(t)

	tests := []struct {
		name         string
		region       string
		category     string
		wantPipeline string
		wantMaxTurns int
	}{
		{"US ecommerce → fast", "US", "ecommerce", "fast", 3},
		{"MENA_SA alcohol → comprehensive", "MENA_SA", "alcohol", "comprehensive", 10},
		{"US healthcare → standard", "US", "healthcare", "standard", 6},
		{"US gambling → comprehensive", "US", "gambling", "comprehensive", 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := matrix.GetReviewPlan(tt.region, tt.category)
			if plan.Pipeline != tt.wantPipeline {
				t.Errorf("Pipeline = %s, want %s", plan.Pipeline, tt.wantPipeline)
			}
			if plan.MaxTurns != tt.wantMaxTurns {
				t.Errorf("MaxTurns = %d, want %d", plan.MaxTurns, tt.wantMaxTurns)
			}
		})
	}
}

func TestReviewEngine_Review_Integration(t *testing.T) {
	matrix := loadTestMatrix(t)
	client := mock.NewLLMClient()
	// Simulate a 2-turn review: analyze → decide.
	client.Responses = append(client.Responses,
		makeToolCallResponse("analyze_content"),
		makeStopResponse("REJECTED", 0.92, []types.PolicyViolation{{
			PolicyID:    "POL_001",
			Severity:    "critical",
			Description: "Unverified medical claim",
			Confidence:  0.92,
			Evidence:    "miracle cure",
		}}),
	)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	engine := NewReviewEngine(client, matrix, mock.ToolDefinitions(), mock.NewToolExecutor(), logger, nil)

	ad := testAd()
	result, err := engine.Review(context.Background(), ad)
	if err != nil {
		t.Fatalf("Review() error: %v", err)
	}

	if result.ExitReason != ExitCompleted {
		t.Errorf("ExitReason = %s, want completed", result.ExitReason)
	}
	if result.ReviewResult == nil {
		t.Fatal("ReviewResult is nil")
	}
	if result.ReviewResult.AdID != ad.ID {
		t.Errorf("AdID = %s, want %s", result.ReviewResult.AdID, ad.ID)
	}

	t.Logf("Decision: %s, Confidence: %.2f, Turns: %d",
		result.ReviewResult.Decision, result.ReviewResult.Confidence, result.State.TurnCount)
	t.Logf("Transitions: %s", result.State.FormatTransitionLog())
}

func TestReviewEngine_Review_NilAd(t *testing.T) {
	matrix := loadTestMatrix(t)
	client := mock.NewLLMClient()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewReviewEngine(client, matrix, mock.ToolDefinitions(), mock.NewToolExecutor(), logger, nil)

	_, err := engine.Review(context.Background(), nil)
	if err == nil {
		t.Error("expected error for nil ad")
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	ad := testAd()
	policies := []types.Policy{
		{ID: "POL_001", Region: "Global", Category: "healthcare", Severity: "critical",
			RuleText: "No unverified medical claims", Source: "TikTok Policy"},
	}
	plan := types.ReviewPlan{
		Pipeline: "standard", MaxTurns: 6, ConfidenceThreshold: 0.70,
		AllowAutoReject: true, RequireVerification: false,
	}

	prompt := buildSystemPrompt(ad, policies, plan, "")

	// Verify prompt contains key sections.
	checks := []string{
		"test_ad_001",        // ad ID
		"Miracle Cure",       // headline
		"healthcare",         // category
		"POL_001",            // policy ID
		"unverified medical", // rule text
		"standard",           // pipeline
		"0.70",               // threshold
		"JSON",               // output format
	}
	for _, check := range checks {
		if !strings.Contains(prompt, check) {
			t.Errorf("system prompt missing %q", check)
		}
	}
}

func TestParseReviewResult_Formats(t *testing.T) {
	state := NewState(testAd())
	config := testConfig()

	tests := []struct {
		name    string
		content string
		wantOk  bool
	}{
		{"raw json", `{"decision":"PASSED","confidence":0.9,"violations":[],"reasoning":"ok"}`, true},
		{"fenced json", "```json\n{\"decision\":\"REJECTED\",\"confidence\":0.8,\"violations\":[],\"reasoning\":\"bad\"}\n```", true},
		{"embedded json", "My review:\n{\"decision\":\"MANUAL_REVIEW\",\"confidence\":0.5,\"violations\":[],\"reasoning\":\"unsure\"}\nDone.", true},
		{"no json", "This is just text", false},
		{"invalid json", `{"decision":`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseReviewResult(tt.content, state, config)
			if tt.wantOk && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tt.wantOk && err == nil {
				t.Error("expected error")
			}
			if tt.wantOk && result != nil {
				t.Logf("Parsed: decision=%s confidence=%.2f", result.Decision, result.Confidence)
			}
		})
	}
}

// TestAdContentParsing_WithEngine verifies we can load test samples and run them through engine.
func TestAdContentParsing_WithEngine(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "data", "samples"))
	if err != nil {
		t.Skip("cannot resolve samples dir")
	}
	data, err := os.ReadFile(filepath.Join(dir, "all_samples.json"))
	if err != nil {
		t.Skip("all_samples.json not found")
	}

	var samples []json.RawMessage
	if err := json.Unmarshal(data, &samples); err != nil {
		t.Fatalf("parsing samples: %v", err)
	}

	// Just verify we can unmarshal each sample as AdContent.
	for i, raw := range samples {
		var sample types.TestAdSample
		if err := json.Unmarshal(raw, &sample); err != nil {
			t.Errorf("sample %d: %v", i, err)
		}
	}
	t.Logf("Successfully parsed %d samples", len(samples))
}
