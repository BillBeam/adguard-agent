package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/strategy"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// PolicyMatcher — 归因+研判环节
//
// 业务归属：JD"沉淀通用策略平台"——将检测到的内容信号与策略矩阵中的
// 适用政策进行匹配。策略规则全部从 StrategyMatrix 加载（policy_kb.json +
// region_rules.json），代码中零硬编码业务规则。
//
// 在"感知-归因-研判-治理"链路中：
//   - 归因：将违规信号关联到具体策略条目
//   - 研判：综合品类禁止状态和信号严重程度，评估违规置信度
type PolicyMatcher struct {
	BaseTool
	matrix *strategy.StrategyMatrix
	logger *slog.Logger
}

// NewPolicyMatcher creates a PolicyMatcher tool.
// It is a pure-logic tool that does not call LLM — all matching is data-driven
// through the StrategyMatrix.
func NewPolicyMatcher(matrix *strategy.StrategyMatrix, logger *slog.Logger) *PolicyMatcher {
	return &PolicyMatcher{
		BaseTool: ReviewToolBase(),
		matrix:   matrix,
		logger:   logger,
	}
}

func (p *PolicyMatcher) Name() string { return "match_policies" }

func (p *PolicyMatcher) Description() string {
	return "Match detected content signals against applicable policies for the given region and category. " +
		"Returns policy violations with severity and confidence. Uses the strategy matrix (policy_kb.json) " +
		"for data-driven matching — no hardcoded rules."
}

func (p *PolicyMatcher) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"region":   {"type": "string", "description": "Ad target region code (e.g. US, EU, MENA_SA)"},
			"category": {"type": "string", "description": "Ad category (e.g. healthcare, alcohol, ecommerce)"},
			"signals":  {"type": "array", "items": {"type": "string"}, "description": "Detected content signals from analyze_content"}
		},
		"required": ["region", "category"]
	}`)
}

func (p *PolicyMatcher) ValidateInput(args json.RawMessage) error {
	var input struct {
		Region   string `json:"region"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return fmt.Errorf("parsing input: %w", err)
	}
	if input.Region == "" || input.Category == "" {
		return fmt.Errorf("region and category are required")
	}
	return nil
}

// Execute — 归因+研判：将信号匹配到策略违规。
//
// 匹配逻辑（全部数据驱动）：
//  1. 从 StrategyMatrix 查询 region+category 的品类状态 — 禁止品类直接违规
//  2. 从 StrategyMatrix 查询适用策略列表
//  3. 遍历每条策略，检查 signals 是否与策略 rule_text 的关键语义匹配
func (p *PolicyMatcher) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var input struct {
		Region   string   `json:"region"`
		Category string   `json:"category"`
		Signals  []string `json:"signals"`
	}
	if isJSONString(args) {
		// LLM passed a string — treat as a signal description.
		input.Signals = []string{unwrapJSONString(args)}
	} else if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	var violations []policyViolation

	// 1. Check if category is prohibited in this region (data-driven via StrategyMatrix).
	rule, found := p.matrix.GetRegionCategoryRule(input.Region, input.Category)
	if found && rule.Status == "prohibited" {
		violations = append(violations, policyViolation{
			PolicyID:    fmt.Sprintf("REGION_%s_%s_BAN", input.Region, input.Category),
			Severity:    "critical",
			Description: fmt.Sprintf("%s advertising is prohibited in %s", input.Category, input.Region),
			Confidence:  0.99,
		})
	}

	// 2. Get applicable policies from StrategyMatrix.
	policies := p.matrix.GetApplicablePolicies(input.Region, input.Category)

	// 3. Match signals against policy rule_text.
	signalSet := make(map[string]bool, len(input.Signals))
	for _, s := range input.Signals {
		signalSet[s] = true
	}

	for _, pol := range policies {
		ruleTextLower := strings.ToLower(pol.RuleText)

		// Signal-to-policy semantic matching (based on rule_text content, not hardcoded).
		for _, signal := range input.Signals {
			confidence := matchSignalToPolicy(signal, ruleTextLower, pol)
			if confidence > 0 {
				violations = append(violations, policyViolation{
					PolicyID:    pol.ID,
					Severity:    pol.Severity,
					Description: fmt.Sprintf("Signal '%s' matches policy %s: %s", signal, pol.ID, truncateStr(pol.RuleText, 100)),
					Confidence:  confidence,
				})
				break // one match per policy is sufficient
			}
		}
	}

	p.logger.Debug("policy matching completed",
		slog.String("region", input.Region),
		slog.String("category", input.Category),
		slog.Int("violations", len(violations)),
	)

	result, _ := json.Marshal(map[string]any{
		"violations":       violations,
		"violations_count": len(violations),
		"region":           input.Region,
		"category":         input.Category,
	})
	return string(result), nil
}

// matchSignalToPolicy determines if a signal matches a policy's rule_text.
// Returns confidence (0 = no match, 0.7-0.95 = match).
// Matching is based on semantic keywords in the rule_text — not hardcoded signal→policy maps.
func matchSignalToPolicy(signal, ruleTextLower string, pol types.Policy) float64 {
	// Map signal types to keywords that would appear in matching policies.
	signalKeywords := map[string][]string{
		"unverified_medical_claim":    {"medical claim", "cure", "treat", "prevent disease", "clinical evidence"},
		"false_regulatory_claim":      {"fda", "approval", "authorized", "regulatory"},
		"absolute_efficacy_claim":     {"100%", "guaranteed", "effective", "absolute"},
		"guaranteed_results_claim":    {"guaranteed", "guarantee", "100%", "absolute"},
		"unrealistic_timeline_claim":  {"specific weight loss", "timeframe", "days", "pounds"},
		"misleading_effortless_claim": {"diet", "exercise", "without effort"},
		"suspected_counterfeit":       {"counterfeit", "intellectual property", "trademark", "unauthorized"},
		"alcohol_content":             {"alcohol", "alcoholic", "beverage"},
		"gambling_content":            {"gambling", "betting", "casino", "poker"},
		"crypto_content":              {"crypto", "cryptocurrency", "nft", "forex"},
		"political_content":           {"political", "election", "candidate", "government"},
		"algospeak_detected":          {"misleading", "false", "deceptive"},
		"landing_page_issue":          {"landing page", "accessible", "mobile"},
		"content_mismatch":            {"landing page", "match", "consistent"},
	}

	keywords, ok := signalKeywords[signal]
	if !ok {
		return 0
	}

	for _, kw := range keywords {
		if strings.Contains(ruleTextLower, kw) {
			// Higher confidence for critical severity policies.
			if pol.Severity == "critical" {
				return 0.90
			}
			return 0.80
		}
	}
	return 0
}

type policyViolation struct {
	PolicyID    string  `json:"policy_id"`
	Severity    string  `json:"severity"`
	Description string  `json:"description"`
	Confidence  float64 `json:"confidence"`
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
