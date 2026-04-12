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

// PolicyKBLookup — 按需查询策略知识库
//
// 业务归属：JD"沉淀通用策略平台"的 Agent 接口。
// 不同角色的 Agent 按需查询策略详情，而非启动时全量加载所有策略文本。
// 查询支持：policy_id 精确查找、region/category 过滤、keyword 全文搜索。
type PolicyKBLookup struct {
	BaseTool
	matrix *strategy.StrategyMatrix
	logger *slog.Logger
}

// NewPolicyKBLookup 创建策略知识库查询工具。
func NewPolicyKBLookup(matrix *strategy.StrategyMatrix, logger *slog.Logger) *PolicyKBLookup {
	return &PolicyKBLookup{
		BaseTool: ReviewToolBase(),
		matrix:   matrix,
		logger:   logger,
	}
}

func (p *PolicyKBLookup) Name() string { return "query_policy_kb" }

func (p *PolicyKBLookup) Description() string {
	return "Query the policy knowledge base for detailed policy rules. " +
		"Supports filtering by policy_id, region, category, or keyword search in rule text. " +
		"Returns full policy details including rule_text, severity, and source. " +
		"Use this tool to look up specific policy requirements before making compliance judgments."
}

func (p *PolicyKBLookup) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"policy_id": {"type": "string", "description": "策略 ID（如 POL_001）"},
			"region":    {"type": "string", "description": "按地区过滤（如 US, EU, MENA_SA）"},
			"category":  {"type": "string", "description": "按品类过滤（如 healthcare, alcohol）"},
			"keyword":   {"type": "string", "description": "在策略原文中搜索关键词（不区分大小写）"}
		}
	}`)
}

func (p *PolicyKBLookup) ValidateInput(args json.RawMessage) error {
	var input policyKBInput
	if isJSONString(args) {
		// LLM 有时直接传字符串，视为 keyword 搜索。
		return nil
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return fmt.Errorf("parsing input: %w", err)
	}
	if input.PolicyID == "" && input.Region == "" && input.Category == "" && input.Keyword == "" {
		return fmt.Errorf("至少需要提供一个查询参数: policy_id, region, category, 或 keyword")
	}
	return nil
}

type policyKBInput struct {
	PolicyID string `json:"policy_id"`
	Region   string `json:"region"`
	Category string `json:"category"`
	Keyword  string `json:"keyword"`
}

func (p *PolicyKBLookup) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var input policyKBInput
	if isJSONString(args) {
		input.Keyword = unwrapJSONString(args)
	} else if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	// 精确 ID 查找：O(1) 直接命中。
	if input.PolicyID != "" {
		if pol, ok := p.matrix.Policies[input.PolicyID]; ok {
			result, _ := json.Marshal([]types.Policy{pol})
			return string(result), nil
		}
		// ID 存在但其他条件也可能匹配，继续扫描。
		if input.Region == "" && input.Category == "" && input.Keyword == "" {
			result, _ := json.Marshal([]types.Policy{})
			return string(result), nil
		}
	}

	// 全量扫描：按 region/category/keyword 组合过滤。
	var matches []types.Policy
	kwLower := strings.ToLower(input.Keyword)

	for _, pol := range p.matrix.Policies {
		if input.PolicyID != "" && pol.ID != input.PolicyID {
			continue
		}
		if input.Region != "" && !policyMatchesRegion(pol, input.Region) {
			continue
		}
		if input.Category != "" && pol.Category != input.Category && pol.Category != "all" {
			continue
		}
		if input.Keyword != "" && !strings.Contains(strings.ToLower(pol.RuleText), kwLower) {
			continue
		}
		matches = append(matches, pol)
	}

	p.logger.Debug("策略知识库查询",
		slog.String("policy_id", input.PolicyID),
		slog.String("region", input.Region),
		slog.String("category", input.Category),
		slog.String("keyword", input.Keyword),
		slog.Int("matches", len(matches)),
	)

	result, _ := json.Marshal(matches)
	return string(result), nil
}

// policyMatchesRegion 检查策略是否匹配目标地区。
// 匹配规则：Global 匹配所有、精确匹配、前缀匹配（策略 "MENA" 匹配输入 "MENA_SA"）。
func policyMatchesRegion(pol types.Policy, region string) bool {
	if pol.Region == "Global" {
		return true
	}
	if pol.Region == region {
		return true
	}
	// 前缀匹配：策略 "MENA" 匹配地区 "MENA_SA"。
	if strings.HasPrefix(region, pol.Region+"_") || strings.HasPrefix(pol.Region, region+"_") {
		return true
	}
	return false
}
