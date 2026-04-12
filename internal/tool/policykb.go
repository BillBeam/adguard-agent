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

// PolicyKBLookup — on-demand policy knowledge base query
//
// Part of the Universal Strategy Platform: the Agent interface for policy lookup.
// Different specialist Agents query policy details on demand instead of bulk-loading
// all policy text at startup.
// Query modes: exact policy_id lookup, region/category filtering, keyword full-text search.
type PolicyKBLookup struct {
	BaseTool
	matrix *strategy.StrategyMatrix
	logger *slog.Logger
}

// NewPolicyKBLookup creates a PolicyKBLookup tool.
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
			"policy_id": {"type": "string", "description": "Policy ID (e.g. POL_001)"},
			"region":    {"type": "string", "description": "Filter by region (e.g. US, EU, MENA_SA)"},
			"category":  {"type": "string", "description": "Filter by category (e.g. healthcare, alcohol)"},
			"keyword":   {"type": "string", "description": "Search keyword in policy rule text (case-insensitive)"}
		}
	}`)
}

func (p *PolicyKBLookup) ValidateInput(args json.RawMessage) error {
	var input policyKBInput
	if isJSONString(args) {
		// LLM sometimes passes a bare string — treat as keyword search.
		return nil
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return fmt.Errorf("parsing input: %w", err)
	}
	if input.PolicyID == "" && input.Region == "" && input.Category == "" && input.Keyword == "" {
		return fmt.Errorf("at least one query parameter is required: policy_id, region, category, or keyword")
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

	// Exact ID lookup: O(1) direct hit.
	if input.PolicyID != "" {
		if pol, ok := p.matrix.Policies[input.PolicyID]; ok {
			result, _ := json.Marshal([]types.Policy{pol})
			return string(result), nil
		}
		// ID present but other filters may also match — continue scanning.
		if input.Region == "" && input.Category == "" && input.Keyword == "" {
			result, _ := json.Marshal([]types.Policy{})
			return string(result), nil
		}
	}

	// Full scan: filter by region/category/keyword combination.
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

	p.logger.Debug("policy KB query",
		slog.String("policy_id", input.PolicyID),
		slog.String("region", input.Region),
		slog.String("category", input.Category),
		slog.String("keyword", input.Keyword),
		slog.Int("matches", len(matches)),
	)

	result, _ := json.Marshal(matches)
	return string(result), nil
}

// policyMatchesRegion checks whether a policy applies to the target region.
// Match rules: Global matches all, exact match, prefix match (policy "MENA" matches input "MENA_SA").
func policyMatchesRegion(pol types.Policy, region string) bool {
	if pol.Region == "Global" {
		return true
	}
	if pol.Region == region {
		return true
	}
	// Prefix match: policy "MENA" matches region "MENA_SA".
	if strings.HasPrefix(region, pol.Region+"_") || strings.HasPrefix(pol.Region, region+"_") {
		return true
	}
	return false
}
