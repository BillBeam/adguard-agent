package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/BillBeam/adguard-agent/internal/strategy"
)

// RegionCompliance — 研判环节
//
// 业务归属：JD"面向全广告形态"的跨区域合规检查——验证广告是否满足
// 目标地区的特定法规要求（MENA Sharia 合规、EU DSA 透明度、US COPPA 儿童保护等）。
//
// 在"感知-归因-研判-治理"链路中：
//   - 研判：基于地区合规规则矩阵（region_rules.json），综合品类状态、
//     法规要求和地区严格度，给出合规/不合规/需审核的判定。
//
// 数据驱动：所有合规规则从 StrategyMatrix 查询，零硬编码。
type RegionCompliance struct {
	BaseTool
	matrix *strategy.StrategyMatrix
	logger *slog.Logger
}

// NewRegionCompliance creates a RegionCompliance tool.
func NewRegionCompliance(matrix *strategy.StrategyMatrix, logger *slog.Logger) *RegionCompliance {
	return &RegionCompliance{
		BaseTool: ReviewToolBase(),
		matrix:   matrix,
		logger:   logger,
	}
}

func (r *RegionCompliance) Name() string { return "check_region_compliance" }

func (r *RegionCompliance) Description() string {
	return "Check whether an ad complies with region-specific regulatory requirements. " +
		"Returns compliance status, applicable requirements, and any missing items. " +
		"Covers MENA Sharia compliance, EU DSA/GDPR, US COPPA/FTC, SEA market regulations."
}

func (r *RegionCompliance) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"region":             {"type": "string", "description": "Ad target region code (e.g. US, EU, MENA_SA, SEA_ID)"},
			"category":           {"type": "string", "description": "Ad category (e.g. healthcare, alcohol, ecommerce)"},
			"ad_content_summary": {"type": "string", "description": "Brief summary of ad content for requirement checking"}
		},
		"required": ["region", "category"]
	}`)
}

func (r *RegionCompliance) ValidateInput(args json.RawMessage) error {
	if isJSONString(args) {
		return nil
	}
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

// Execute — 研判：地区合规检查。
//
// 逻辑（全部数据驱动，从 StrategyMatrix 查询）：
//  1. GetRegionCategoryRule → status/requirements
//  2. prohibited → non_compliant（品类在该地区被禁止）
//  3. restricted → needs_review（列出 requirements，标记缺失项）
//  4. permitted → compliant
//  5. GetRegionStrictness → 地区严格度（strict/standard）
func (r *RegionCompliance) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var input struct {
		Region           string `json:"region"`
		Category         string `json:"category"`
		AdContentSummary string `json:"ad_content_summary"`
	}
	if isJSONString(args) {
		input.AdContentSummary = unwrapJSONString(args)
	} else if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	strictness := r.matrix.GetRegionStrictness(input.Region)
	riskLevel := r.matrix.GetRiskLevel(input.Category)

	rule, found := r.matrix.GetRegionCategoryRule(input.Region, input.Category)

	var status, ruleStatus, notes string
	var requirements []string
	var missingItems []string

	if !found {
		// Unknown region/category combination — fail-closed: needs review.
		status = "needs_review"
		ruleStatus = "unknown"
		notes = fmt.Sprintf("No specific rules found for %s/%s. Manual review recommended.", input.Region, input.Category)
	} else {
		ruleStatus = rule.Status
		requirements = rule.Requirements
		notes = rule.Notes

		switch rule.Status {
		case "prohibited":
			status = "non_compliant"
			missingItems = append(missingItems, fmt.Sprintf("%s is prohibited in %s", input.Category, input.Region))
		case "restricted":
			status = "needs_review"
			// All requirements are considered potentially missing until verified.
			missingItems = append(missingItems, requirements...)
		case "permitted":
			status = "compliant"
		case "prohibited_brand_content":
			status = "needs_review"
			missingItems = append(missingItems, "category prohibited in branded content, check ad type")
		default:
			status = "needs_review"
		}
	}

	r.logger.Debug("region compliance check",
		slog.String("region", input.Region),
		slog.String("category", input.Category),
		slog.String("status", status),
		slog.String("strictness", strictness),
	)

	result, _ := json.Marshal(map[string]any{
		"status":            status,
		"region_strictness": strictness,
		"risk_level":        string(riskLevel),
		"rule_status":       ruleStatus,
		"requirements":      requirements,
		"missing_items":     missingItems,
		"notes":             notes,
	})
	return string(result), nil
}
