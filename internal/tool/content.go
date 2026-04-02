package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/strategy"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// ContentAnalyzer — 感知环节核心
//
// 业务归属：JD"面向日均亿级别广告素材和全广告形态"的内容检测能力。
// 检测广告文案中的违规信号——虚假声明、绝对化用语、误导性 CTA、
// Algospeak（算法语：谐音字/特殊符号/Unicode 替换规避检测）。
//
// 在"感知-归因-研判-治理"链路中：
//   - 感知：这是链路的第一站，负责从广告素材中提取违规信号
//   - 输出的 signals 列表将被 PolicyMatcher 用于归因环节
//
// 实现方式：LLM 驱动的语义分析，prompt 中嵌入从 StrategyMatrix 获取的
// 适用政策 rule_text。LLM 调用失败时降级为关键词匹配（fail-closed）。
type ContentAnalyzer struct {
	BaseTool
	client llm.LLMClient
	matrix *strategy.StrategyMatrix
	logger *slog.Logger
}

// NewContentAnalyzer creates a ContentAnalyzer tool.
func NewContentAnalyzer(client llm.LLMClient, matrix *strategy.StrategyMatrix, logger *slog.Logger) *ContentAnalyzer {
	return &ContentAnalyzer{
		BaseTool: ReviewToolBase(),
		client:   client,
		matrix:   matrix,
		logger:   logger,
	}
}

func (c *ContentAnalyzer) Name() string { return "analyze_content" }

func (c *ContentAnalyzer) Description() string {
	return "Analyze ad content for policy violations including false claims, misleading language, " +
		"absolute/unverified claims, and Algospeak (algorithmic language evasion using homoglyphs, " +
		"special characters, or Unicode substitution). Returns detected signals with severity and evidence."
}

func (c *ContentAnalyzer) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"headline":          {"type": "string", "description": "Ad headline text"},
			"body":              {"type": "string", "description": "Ad body text"},
			"cta":               {"type": "string", "description": "Call-to-action text"},
			"image_description": {"type": "string", "description": "Image description if present"},
			"category":          {"type": "string", "description": "Ad category for context-aware analysis"},
			"ad_type":           {"type": "string", "description": "Ad format type: text, image_text, video"}
		},
		"required": ["headline", "body"]
	}`)
}

func (c *ContentAnalyzer) ValidateInput(args json.RawMessage) error {
	// Accept both object {"headline":"..."} and plain string "..." (LLM may pass either).
	if isJSONString(args) {
		return nil // will be handled in Execute
	}
	var input struct {
		Headline string `json:"headline"`
		Body     string `json:"body"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return fmt.Errorf("parsing input: %w", err)
	}
	if input.Headline == "" && input.Body == "" {
		return fmt.Errorf("at least one of headline or body is required")
	}
	return nil
}

// Execute — 感知：LLM 驱动的广告内容分析。
//
// 流程：
//  1. 从 StrategyMatrix 获取适用政策 rule_text
//  2. 构建 LLM prompt（含 Algospeak 检测指引 + ad_type 适配）
//  3. 调用 LLM 获取分析结果
//  4. 解析 LLM JSON 输出
//  5. LLM 失败时降级为关键词匹配
func (c *ContentAnalyzer) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input struct {
		Headline         string `json:"headline"`
		Body             string `json:"body"`
		CTA              string `json:"cta"`
		ImageDescription string `json:"image_description"`
		Category         string `json:"category"`
		AdType           string `json:"ad_type"`
	}
	if isJSONString(args) {
		// LLM passed a plain string — treat as body text.
		input.Body = unwrapJSONString(args)
	} else if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	// 1. Get applicable policies for prompt context.
	category := input.Category
	if category == "" {
		category = "all"
	}
	policies := c.matrix.GetApplicablePolicies("Global", category)

	// 2. Build LLM prompt.
	prompt := c.buildAnalysisPrompt(input.Headline, input.Body, input.CTA, input.ImageDescription, input.Category, input.AdType, policies)

	// 3. Call LLM.
	resp, err := c.client.ChatCompletion(ctx, types.ChatCompletionRequest{
		Messages: []types.Message{
			{Role: types.RoleSystem, Content: types.NewTextContent(prompt)},
			{Role: types.RoleUser, Content: types.NewTextContent("Analyze the ad content above and return the JSON result.")},
		},
	})

	if err != nil {
		c.logger.Warn("LLM call failed for content analysis, falling back to rule-based",
			slog.String("error", err.Error()),
		)
		return c.fallbackRuleBased(input.Headline, input.Body, input.CTA, input.Category)
	}

	if len(resp.Choices) == 0 {
		return c.fallbackRuleBased(input.Headline, input.Body, input.CTA, input.Category)
	}

	// 4. Parse LLM output.
	raw := resp.Choices[0].Message.Content.String()
	jsonStr := extractJSONFromContent(raw)
	if jsonStr != "" && json.Valid([]byte(jsonStr)) {
		return jsonStr, nil
	}

	c.logger.Warn("LLM output not valid JSON, falling back to rule-based",
		slog.String("raw", truncateStr(raw, 200)),
	)
	return c.fallbackRuleBased(input.Headline, input.Body, input.CTA, input.Category)
}

// buildAnalysisPrompt constructs the LLM prompt with policy context,
// Algospeak detection guidance, and ad-type-specific instructions.
func (c *ContentAnalyzer) buildAnalysisPrompt(headline, body, cta, imageDesc, category, adType string, policies []types.Policy) string {
	var b strings.Builder

	b.WriteString("You are an ad content safety analyst for an international advertising platform.\n\n")

	// Ad content.
	b.WriteString("=== AD CONTENT TO ANALYZE ===\n")
	fmt.Fprintf(&b, "Headline: %s\n", headline)
	fmt.Fprintf(&b, "Body: %s\n", body)
	if cta != "" {
		fmt.Fprintf(&b, "CTA: %s\n", cta)
	}
	if imageDesc != "" {
		fmt.Fprintf(&b, "Image Description: %s\n", imageDesc)
	}
	if category != "" {
		fmt.Fprintf(&b, "Category: %s\n", category)
	}
	b.WriteString("\n")

	// Ad type-specific detection guidance.
	b.WriteString("=== AD TYPE ANALYSIS FOCUS ===\n")
	switch adType {
	case "image_text":
		b.WriteString("This is an IMAGE+TEXT ad. Pay special attention to:\n")
		b.WriteString("- Text-image inconsistency (text claims vs image content)\n")
		b.WriteString("- Violations hidden in image description that bypass text-only detection\n")
		b.WriteString("- Visual cues suggesting prohibited content (fake logos, misleading before/after)\n\n")
	case "video":
		b.WriteString("This is a VIDEO ad. Pay special attention to:\n")
		b.WriteString("- Claims made in narration/voiceover that differ from text overlay\n")
		b.WriteString("- Rapid text flashes that hide disclaimers\n")
		b.WriteString("- Visual demonstration claims that may be staged/misleading\n\n")
	default: // "text" or unspecified
		b.WriteString("This is a TEXT ad. Focus on textual claims and language analysis.\n\n")
	}

	// Algospeak detection guidance.
	b.WriteString("=== ALGOSPEAK DETECTION ===\n")
	b.WriteString("Algospeak is the use of code words, homoglyphs, special characters, or Unicode\n")
	b.WriteString("substitutions to evade automated content detection. Watch for:\n")
	b.WriteString("- Homoglyphs: Cyrillic/Greek letters replacing Latin (е→e, а→a, о→o)\n")
	b.WriteString("- Leet speak: s3x, w3ight l0ss, c@sino, g@mbling\n")
	b.WriteString("- Zero-width characters inserted to break keyword matching\n")
	b.WriteString("- Emoji substitution: 💊 for pills, 🎰 for gambling, 🍷 for alcohol\n")
	b.WriteString("- Phonetic substitution: 'wei減' for weight loss, 'cr¥pto' for crypto\n")
	b.WriteString("- Deliberate misspelling: 'diabeetus cure', 'marij uana'\n")
	b.WriteString("- If Algospeak is detected, report the signal as 'algospeak_detected' with\n")
	b.WriteString("  the decoded meaning as evidence.\n\n")

	// Applicable policies.
	if len(policies) > 0 {
		b.WriteString("=== APPLICABLE POLICIES ===\n")
		for _, p := range policies {
			fmt.Fprintf(&b, "[%s] %s\n", p.ID, truncateStr(p.RuleText, 200))
		}
		b.WriteString("\n")
	}

	// Output format.
	b.WriteString("=== OUTPUT FORMAT ===\n")
	b.WriteString("Return ONLY a JSON object with this structure:\n")
	b.WriteString(`{"signals":[{"signal":"signal_type","severity":"critical|high|moderate|info","evidence":"what triggered this"}],"signals_count":N}`)
	b.WriteString("\n\nCommon signal types: unverified_medical_claim, false_regulatory_claim, ")
	b.WriteString("absolute_efficacy_claim, guaranteed_results_claim, unrealistic_timeline_claim, ")
	b.WriteString("misleading_effortless_claim, suspected_counterfeit, algospeak_detected, ")
	b.WriteString("no_issues_detected\n")

	return b.String()
}

// fallbackRuleBased provides rule-based content analysis when LLM is unavailable.
// This matches the mock/tools.go handleAnalyzeContent logic.
func (c *ContentAnalyzer) fallbackRuleBased(headline, body, cta, category string) (string, error) {
	text := strings.ToLower(headline + " " + body + " " + cta)

	type signal struct {
		Signal   string `json:"signal"`
		Severity string `json:"severity"`
		Evidence string `json:"evidence"`
	}
	var signals []signal

	patterns := map[string]struct {
		signalType string
		severity   string
	}{
		"cure":         {"unverified_medical_claim", "critical"},
		"miracle":      {"unverified_medical_claim", "critical"},
		"100% effect":  {"absolute_efficacy_claim", "critical"},
		"guaranteed":   {"guaranteed_results_claim", "critical"},
		"fda approved": {"false_regulatory_claim", "critical"},
		"90% off":      {"suspected_counterfeit", "high"},
		"no diet":      {"misleading_effortless_claim", "high"},
		"no exercise":  {"misleading_effortless_claim", "high"},
	}

	for pattern, info := range patterns {
		if strings.Contains(text, pattern) {
			signals = append(signals, signal{
				Signal:   info.signalType,
				Severity: info.severity,
				Evidence: fmt.Sprintf("Text contains '%s'", pattern),
			})
		}
	}

	if (category == "weight_loss" || strings.Contains(text, "lose") || strings.Contains(text, "weight")) &&
		(strings.Contains(text, "days") || strings.Contains(text, "week")) {
		signals = append(signals, signal{
			Signal:   "unrealistic_timeline_claim",
			Severity: "critical",
			Evidence: "Weight loss claim with specific timeframe",
		})
	}

	if len(signals) == 0 {
		signals = append(signals, signal{
			Signal:   "no_issues_detected",
			Severity: "info",
			Evidence: "Content appears compliant (rule-based fallback)",
		})
	}

	result, _ := json.Marshal(map[string]any{
		"signals":       signals,
		"signals_count": len(signals),
	})
	return string(result), nil
}

// extractJSONFromContent extracts a JSON object from LLM output text.
func extractJSONFromContent(content string) string {
	content = strings.TrimSpace(content)
	if json.Valid([]byte(content)) {
		return content
	}
	if idx := strings.Index(content, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(content[start:], "```"); end >= 0 {
			candidate := strings.TrimSpace(content[start : start+end])
			if json.Valid([]byte(candidate)) {
				return candidate
			}
		}
	}
	if start := strings.Index(content, "{"); start >= 0 {
		if end := strings.LastIndex(content, "}"); end > start {
			candidate := content[start : end+1]
			if json.Valid([]byte(candidate)) {
				return candidate
			}
		}
	}
	return ""
}
