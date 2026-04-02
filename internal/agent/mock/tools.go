package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// ToolExecutor is a mock tool executor that simulates ad review tools.
// Each tool accepts structured inputs aligned with Phase 0 types and returns
// realistic review signals aligned with types.PolicyViolation.
type ToolExecutor struct {
	handlers map[string]toolHandler
}

type toolHandler func(ctx context.Context, args json.RawMessage) (string, error)

// NewToolExecutor creates a mock executor with three review tools.
func NewToolExecutor() *ToolExecutor {
	e := &ToolExecutor{handlers: make(map[string]toolHandler)}
	e.handlers["analyze_content"] = handleAnalyzeContent
	e.handlers["match_policies"] = handleMatchPolicies
	e.handlers["check_landing_page"] = handleCheckLandingPage
	return e
}

// Execute runs the requested tool calls and returns tool_result messages.
func (e *ToolExecutor) Execute(ctx context.Context, toolCalls []types.ToolCall) ([]types.Message, error) {
	results := make([]types.Message, 0, len(toolCalls))
	for _, tc := range toolCalls {
		handler, ok := e.handlers[tc.Function.Name]
		if !ok {
			results = append(results, types.Message{
				Role:       types.RoleTool,
				Content:    types.NewTextContent(fmt.Sprintf(`{"error":"unknown tool %q"}`, tc.Function.Name)),
				ToolCallID: tc.ID,
			})
			continue
		}

		output, err := handler(ctx, tc.Function.Arguments)
		if err != nil {
			output = fmt.Sprintf(`{"error":"%s"}`, err.Error())
		}

		results = append(results, types.Message{
			Role:       types.RoleTool,
			Content:    types.NewTextContent(output),
			ToolCallID: tc.ID,
		})
	}
	return results, nil
}

// ToolDefinitions returns the tool definitions for the mock tools.
func ToolDefinitions() []types.ToolDefinition {
	return []types.ToolDefinition{
		{
			Type: "function",
			Function: types.FunctionSpec{
				Name:        "analyze_content",
				Description: "Analyze ad content for problematic claims, misleading language, and policy signals. Input is the ad body (headline, body text, CTA, optional image description). Returns detected issues with severity and evidence.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"headline":          {"type": "string", "description": "Ad headline text"},
						"body":              {"type": "string", "description": "Ad body text"},
						"cta":               {"type": "string", "description": "Call-to-action text"},
						"image_description": {"type": "string", "description": "Image description if present"},
						"category":          {"type": "string", "description": "Ad category for context-aware analysis"}
					},
					"required": ["headline", "body"]
				}`),
			},
		},
		{
			Type: "function",
			Function: types.FunctionSpec{
				Name:        "match_policies",
				Description: "Match detected content signals against applicable policies for the given region and category. Returns matched policies with violation details.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"region":   {"type": "string", "description": "Ad target region code (e.g. US, EU, MENA_SA)"},
						"category": {"type": "string", "description": "Ad category (e.g. healthcare, alcohol, ecommerce)"},
						"signals":  {"type": "array", "items": {"type": "string"}, "description": "Detected content signals from analyze_content"}
					},
					"required": ["region", "category"]
				}`),
			},
		},
		{
			Type: "function",
			Function: types.FunctionSpec{
				Name:        "check_landing_page",
				Description: "Check the ad landing page for compliance issues including accessibility, content consistency, required disclosures, and data collection practices.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"url":                 {"type": "string", "description": "Landing page URL"},
						"description":         {"type": "string", "description": "Landing page content description"},
						"is_accessible":       {"type": "boolean", "description": "Whether the page is accessible"},
						"is_mobile_optimized": {"type": "boolean", "description": "Whether the page is mobile optimized"},
						"ad_category":         {"type": "string", "description": "Ad category for category-specific checks"}
					},
					"required": ["url"]
				}`),
			},
		},
	}
}

// --- Mock tool handlers ---

// handleAnalyzeContent simulates content analysis by detecting problematic claim patterns.
// Input aligns with types.AdBody fields.
func handleAnalyzeContent(_ context.Context, args json.RawMessage) (string, error) {
	var input struct {
		Headline         string `json:"headline"`
		Body             string `json:"body"`
		CTA              string `json:"cta"`
		ImageDescription string `json:"image_description"`
		Category         string `json:"category"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("parsing analyze_content input: %w", err)
	}

	text := strings.ToLower(input.Headline + " " + input.Body + " " + input.CTA)

	type signal struct {
		Signal   string `json:"signal"`
		Severity string `json:"severity"`
		Evidence string `json:"evidence"`
	}
	var signals []signal

	// Detect absolute/unverified medical claims.
	claimPatterns := map[string]string{
		"cure":        "unverified_medical_claim",
		"miracle":     "unverified_medical_claim",
		"100% effect": "absolute_efficacy_claim",
		"guaranteed":  "guaranteed_results_claim",
		"fda approved": "false_regulatory_claim",
	}
	for pattern, sigType := range claimPatterns {
		if strings.Contains(text, pattern) {
			signals = append(signals, signal{
				Signal:   sigType,
				Severity: "critical",
				Evidence: fmt.Sprintf("Text contains '%s'", pattern),
			})
		}
	}

	// Detect weight loss specific claims.
	if input.Category == "weight_loss" || strings.Contains(text, "lose") || strings.Contains(text, "weight") {
		if strings.Contains(text, "days") || strings.Contains(text, "week") {
			signals = append(signals, signal{
				Signal:   "unrealistic_timeline_claim",
				Severity: "critical",
				Evidence: "Weight loss claim with specific timeframe",
			})
		}
	}

	// Detect misleading patterns.
	if strings.Contains(text, "no diet") || strings.Contains(text, "no exercise") {
		signals = append(signals, signal{
			Signal:   "misleading_effortless_claim",
			Severity: "high",
			Evidence: "Claims results without effort",
		})
	}

	// Detect counterfeit indicators.
	if strings.Contains(text, "90% off") || strings.Contains(text, "95% off") {
		signals = append(signals, signal{
			Signal:   "suspected_counterfeit_pricing",
			Severity: "high",
			Evidence: "Unrealistically high discount for claimed premium goods",
		})
	}

	if len(signals) == 0 {
		signals = append(signals, signal{
			Signal:   "no_issues_detected",
			Severity: "info",
			Evidence: "Content appears compliant",
		})
	}

	result, _ := json.Marshal(map[string]any{
		"signals":       signals,
		"signals_count": len(signals),
	})
	return string(result), nil
}

// handleMatchPolicies simulates policy matching based on region, category, and detected signals.
func handleMatchPolicies(_ context.Context, args json.RawMessage) (string, error) {
	var input struct {
		Region   string   `json:"region"`
		Category string   `json:"category"`
		Signals  []string `json:"signals"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("parsing match_policies input: %w", err)
	}

	type violation struct {
		PolicyID    string  `json:"policy_id"`
		Severity    string  `json:"severity"`
		Description string  `json:"description"`
		Confidence  float64 `json:"confidence"`
	}
	var violations []violation

	// Category-specific policy matching.
	switch input.Category {
	case "alcohol":
		if strings.HasPrefix(input.Region, "MENA") || input.Region == "SEA_ID" {
			violations = append(violations, violation{
				PolicyID:    "POL_005",
				Severity:    "critical",
				Description: "Alcohol advertising prohibited in " + input.Region,
				Confidence:  0.99,
			})
		}
	case "gambling":
		violations = append(violations, violation{
			PolicyID:    "POL_008",
			Severity:    "critical",
			Description: "Gambling ads prohibited by default globally",
			Confidence:  0.95,
		})
	case "crypto":
		violations = append(violations, violation{
			PolicyID:    "POL_009",
			Severity:    "critical",
			Description: "Crypto ads prohibited in branded content",
			Confidence:  0.95,
		})
	}

	// Signal-based matching.
	for _, sig := range input.Signals {
		switch sig {
		case "unverified_medical_claim":
			violations = append(violations, violation{
				PolicyID:    "POL_001",
				Severity:    "critical",
				Description: "Unverified medical claim detected",
				Confidence:  0.90,
			})
		case "false_regulatory_claim":
			violations = append(violations, violation{
				PolicyID:    "POL_002",
				Severity:    "critical",
				Description: "False FDA/regulatory approval claim",
				Confidence:  0.95,
			})
		case "guaranteed_results_claim":
			violations = append(violations, violation{
				PolicyID:    "POL_018",
				Severity:    "moderate",
				Description: "Absolute/guaranteed claims prohibited",
				Confidence:  0.85,
			})
		}
	}

	result, _ := json.Marshal(map[string]any{
		"violations":       violations,
		"violations_count": len(violations),
		"region":           input.Region,
		"category":         input.Category,
	})
	return string(result), nil
}

// handleCheckLandingPage simulates landing page checks.
// Input aligns with types.LandingPage fields.
func handleCheckLandingPage(_ context.Context, args json.RawMessage) (string, error) {
	var input struct {
		URL              string `json:"url"`
		Description      string `json:"description"`
		IsAccessible     *bool  `json:"is_accessible"`
		IsMobileOptimized *bool `json:"is_mobile_optimized"`
		AdCategory       string `json:"ad_category"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("parsing check_landing_page input: %w", err)
	}

	type issue struct {
		Issue    string `json:"issue"`
		Severity string `json:"severity"`
		Detail   string `json:"detail"`
	}
	var issues []issue

	// Check accessibility.
	if input.IsAccessible != nil && !*input.IsAccessible {
		issues = append(issues, issue{
			Issue:    "landing_page_not_accessible",
			Severity: "high",
			Detail:   "Landing page returns error or is unreachable",
		})
	}

	// Check mobile optimization.
	if input.IsMobileOptimized != nil && !*input.IsMobileOptimized {
		issues = append(issues, issue{
			Issue:    "not_mobile_optimized",
			Severity: "moderate",
			Detail:   "Landing page not optimized for mobile devices",
		})
	}

	// Check description for red flags.
	desc := strings.ToLower(input.Description)
	if strings.Contains(desc, "404") || strings.Contains(desc, "not found") {
		issues = append(issues, issue{
			Issue:    "landing_page_404",
			Severity: "high",
			Detail:   "Landing page returns 404 Not Found",
		})
	}
	if strings.Contains(desc, "different product") || strings.Contains(desc, "completely different") {
		issues = append(issues, issue{
			Issue:    "content_mismatch",
			Severity: "high",
			Detail:   "Landing page content does not match ad creative",
		})
	}
	if strings.Contains(desc, "no privacy policy") || strings.Contains(desc, "missing privacy") {
		issues = append(issues, issue{
			Issue:    "missing_privacy_policy",
			Severity: "moderate",
			Detail:   "Landing page lacks required privacy policy",
		})
	}
	if strings.Contains(desc, "fake") || strings.Contains(desc, "counterfeit") {
		issues = append(issues, issue{
			Issue:    "suspected_counterfeit",
			Severity: "critical",
			Detail:   "Landing page indicators suggest counterfeit goods",
		})
	}
	if strings.Contains(desc, "ssn") || strings.Contains(desc, "social security") {
		issues = append(issues, issue{
			Issue:    "excessive_data_collection",
			Severity: "critical",
			Detail:   "Landing page requests sensitive personal information",
		})
	}

	if len(issues) == 0 {
		issues = append(issues, issue{
			Issue:    "no_issues_detected",
			Severity: "info",
			Detail:   "Landing page appears compliant",
		})
	}

	result, _ := json.Marshal(map[string]any{
		"issues":       issues,
		"issues_count": len(issues),
		"url":          input.URL,
	})
	return string(result), nil
}
