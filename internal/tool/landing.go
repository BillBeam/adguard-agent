package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// LandingPageChecker — 感知环节
//
// 业务归属：JD"面向全广告形态"的落地页审核——TikTok 广告最高频拒绝原因
// 之一就是落地页问题（不可访问、素材与落地页不一致、缺少隐私政策、
// 要求敏感信息）。
//
// 在"感知-归因-研判-治理"链路中：
//   - 感知：检测落地页的合规问题信号（可访问性、移动端适配、内容一致性、
//     数据收集合规）
//
// 实现方式：规则层（可访问性/移动端/关键词检测）+ LLM 层（内容一致性检查）。
// LLM 调用失败时仅返回规则层结果（降级不阻塞）。
type LandingPageChecker struct {
	BaseTool
	client llm.LLMClient
	logger *slog.Logger
}

// NewLandingPageChecker creates a LandingPageChecker tool.
func NewLandingPageChecker(client llm.LLMClient, logger *slog.Logger) *LandingPageChecker {
	return &LandingPageChecker{
		// 100KB limit: landing pages can be large (50-200KB HTML).
		// The ResultBudget handles smart persist+preview; this limit is the
		// upper bound before even budget-based handling kicks in.
		BaseTool: BaseTool{
			concurrencySafe: true,
			readOnly:        true,
			maxResultSize:   102400, // 100KB
		},
		client: client,
		logger: logger,
	}
}

func (l *LandingPageChecker) Name() string { return "check_landing_page" }

func (l *LandingPageChecker) Description() string {
	return "Check the ad landing page for compliance issues including accessibility, " +
		"content consistency with the ad creative, required disclosures (privacy policy, terms), " +
		"mobile optimization, and data collection practices."
}

func (l *LandingPageChecker) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url":                 {"type": "string", "description": "Landing page URL"},
			"description":         {"type": "string", "description": "Landing page content description"},
			"is_accessible":       {"type": "boolean", "description": "Whether the page is accessible"},
			"is_mobile_optimized": {"type": "boolean", "description": "Whether the page is mobile optimized"},
			"ad_category":         {"type": "string", "description": "Ad category for category-specific checks"},
			"ad_headline":         {"type": "string", "description": "Ad headline for consistency check"},
			"ad_body":             {"type": "string", "description": "Ad body text for consistency check"}
		},
		"required": ["url"]
	}`)
}

func (l *LandingPageChecker) ValidateInput(args json.RawMessage) error {
	var input struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return fmt.Errorf("parsing input: %w", err)
	}
	if input.URL == "" {
		return fmt.Errorf("url is required")
	}
	return nil
}

// Execute — 感知：落地页合规检查（规则层 + LLM 内容一致性层）。
func (l *LandingPageChecker) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input struct {
		URL               string `json:"url"`
		Description       string `json:"description"`
		IsAccessible      *bool  `json:"is_accessible"`
		IsMobileOptimized *bool  `json:"is_mobile_optimized"`
		AdCategory        string `json:"ad_category"`
		AdHeadline        string `json:"ad_headline"`
		AdBody            string `json:"ad_body"`
	}
	if isJSONString(args) {
		input.URL = unwrapJSONString(args)
	} else if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	type issue struct {
		Issue    string `json:"issue"`
		Severity string `json:"severity"`
		Detail   string `json:"detail"`
	}
	var issues []issue

	// === Rule-based checks (no LLM) ===

	// Accessibility check.
	if input.IsAccessible != nil && !*input.IsAccessible {
		issues = append(issues, issue{
			Issue:    "landing_page_not_accessible",
			Severity: "high",
			Detail:   "Landing page is not accessible or returns error",
		})
	}

	// Mobile optimization check.
	if input.IsMobileOptimized != nil && !*input.IsMobileOptimized {
		issues = append(issues, issue{
			Issue:    "not_mobile_optimized",
			Severity: "moderate",
			Detail:   "Landing page not optimized for mobile devices",
		})
	}

	// Description keyword checks.
	if input.Description != "" {
		desc := strings.ToLower(input.Description)

		keywordChecks := []struct {
			keywords []string
			issue    string
			severity string
			detail   string
		}{
			{[]string{"404", "not found"}, "landing_page_404", "high", "Landing page returns 404 Not Found"},
			{[]string{"different product", "completely different"}, "content_mismatch", "high", "Landing page content does not match ad creative"},
			{[]string{"no privacy policy", "missing privacy"}, "missing_privacy_policy", "moderate", "Landing page lacks required privacy policy"},
			{[]string{"ssn", "social security"}, "excessive_data_collection", "critical", "Landing page requests sensitive personal information"},
			{[]string{"auto-download", "auto download"}, "auto_download", "high", "Landing page triggers automatic downloads"},
			{[]string{"fake", "counterfeit"}, "suspected_counterfeit", "critical", "Landing page indicators suggest counterfeit goods"},
			{[]string{"no ingredient", "no return"}, "missing_disclosures", "moderate", "Landing page missing required product disclosures"},
		}

		for _, check := range keywordChecks {
			for _, kw := range check.keywords {
				if strings.Contains(desc, kw) {
					issues = append(issues, issue{
						Issue:    check.issue,
						Severity: check.severity,
						Detail:   check.detail,
					})
					break
				}
			}
		}
	}

	// === LLM-based consistency check (optional, degradable) ===
	if input.AdHeadline != "" && input.Description != "" {
		consistencyIssue := l.checkConsistency(ctx, input.AdHeadline, input.AdBody, input.Description)
		if consistencyIssue != nil {
			issues = append(issues, *consistencyIssue)
		}
	}

	if len(issues) == 0 {
		issues = append(issues, issue{
			Issue:    "no_issues_detected",
			Severity: "info",
			Detail:   "Landing page appears compliant",
		})
	}

	// Key signal summary: always inline even if full result is persisted to disk.
	// These signals give the LLM enough context for judgment without the full HTML.
	privacyFound := input.Description != "" && strings.Contains(strings.ToLower(input.Description), "privacy")

	result, _ := json.Marshal(map[string]any{
		"issues":               issues,
		"issues_count":         len(issues),
		"url":                  input.URL,
		"privacy_policy_found": privacyFound,
		"is_accessible":        input.IsAccessible,
		"is_mobile_optimized":  input.IsMobileOptimized,
	})
	return string(result), nil
}

// checkConsistency uses LLM to verify ad-to-landing-page content consistency.
// Returns an issue if inconsistency is detected, nil otherwise.
// LLM failure is silently ignored (degradable — returns nil, doesn't block).
func (l *LandingPageChecker) checkConsistency(ctx context.Context, adHeadline, adBody, lpDescription string) *struct {
	Issue    string `json:"issue"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
} {
	prompt := fmt.Sprintf(`Compare this ad creative with its landing page description.
Are they promoting the same product/service?

Ad Headline: %s
Ad Body: %s
Landing Page Description: %s

Return ONLY a JSON object: {"consistent": true/false, "mismatch_details": "explanation if inconsistent"}`,
		adHeadline, adBody, lpDescription)

	resp, err := l.client.ChatCompletion(ctx, types.ChatCompletionRequest{
		Messages: []types.Message{
			{Role: types.RoleUser, Content: types.NewTextContent(prompt)},
		},
	})
	if err != nil {
		l.logger.Debug("LLM consistency check failed, skipping", slog.String("error", err.Error()))
		return nil
	}
	if len(resp.Choices) == 0 {
		return nil
	}

	raw := resp.Choices[0].Message.Content.String()
	jsonStr := extractJSONFromContent(raw)
	if jsonStr == "" {
		return nil
	}

	var result struct {
		Consistent      bool   `json:"consistent"`
		MismatchDetails string `json:"mismatch_details"`
	}
	if json.Unmarshal([]byte(jsonStr), &result) != nil {
		return nil
	}

	if !result.Consistent {
		return &struct {
			Issue    string `json:"issue"`
			Severity string `json:"severity"`
			Detail   string `json:"detail"`
		}{
			Issue:    "content_mismatch",
			Severity: "high",
			Detail:   "LLM detected ad-landing page inconsistency: " + result.MismatchDetails,
		}
	}
	return nil
}
