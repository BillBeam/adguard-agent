// Package strategy implements the data-driven strategy matrix — the core of
// the "Universal Strategy Platform" described in the system architecture.
//
// The matrix cross-references policies, regional rules, and category risk levels
// to answer: "Given an ad's region and category, what policies apply, how risky
// is it, and what review pipeline should we use?"
//
// Zero business rules are hardcoded. All decisions derive from loaded JSON data.
package strategy

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// StrategyMatrix is the data-driven core of all review logic.
// It is constructed from three JSON data files and provides query methods
// consumed by the orchestration engine and review agents.
type StrategyMatrix struct {
	Policies     map[string]types.Policy                      // policy ID → policy
	RegionRules  *types.RegionRules                           // region → category → rule
	CategoryRisk map[string]types.RiskLevel                   // ad category → risk level
	logger       *slog.Logger
}

// NewStrategyMatrix loads the strategy matrix from JSON data files.
// All three files are required; any error is fatal (fail-fast).
func NewStrategyMatrix(policyPath, regionRulesPath, categoryRiskPath string, logger *slog.Logger) (*StrategyMatrix, error) {
	if logger == nil {
		logger = slog.Default()
	}

	policies, err := loadPolicies(policyPath)
	if err != nil {
		return nil, fmt.Errorf("loading policies from %s: %w", policyPath, err)
	}

	regionRules, err := loadRegionRules(regionRulesPath)
	if err != nil {
		return nil, fmt.Errorf("loading region rules from %s: %w", regionRulesPath, err)
	}

	categoryRisk, err := loadCategoryRisk(categoryRiskPath)
	if err != nil {
		return nil, fmt.Errorf("loading category risk from %s: %w", categoryRiskPath, err)
	}

	m := &StrategyMatrix{
		Policies:     policies,
		RegionRules:  regionRules,
		CategoryRisk: categoryRisk,
		logger:       logger,
	}

	logger.Info("strategy matrix loaded",
		slog.Int("policies", len(policies)),
		slog.Int("regions", len(regionRules.Rules)),
		slog.Int("risk_categories", len(categoryRisk)),
	)

	return m, nil
}

// GetApplicablePolicies returns all policies that apply to a given region+category.
//
// Matching logic:
//  1. Policies with region="Global" or region matching the exact region code
//  2. Policies with category="all" (apply to every ad category)
//  3. Policies with category matching the exact ad category
//  4. Region prefix matching: policy region "EU" matches ad region "EU_DE"
func (m *StrategyMatrix) GetApplicablePolicies(region, category string) []types.Policy {
	var applicable []types.Policy

	for _, p := range m.Policies {
		if !m.policyMatchesCategory(p, category) {
			continue
		}
		if !m.policyMatchesRegion(p, region) {
			continue
		}
		applicable = append(applicable, p)
	}

	m.logger.Debug("applicable policies resolved",
		slog.String("region", region),
		slog.String("category", category),
		slog.Int("count", len(applicable)),
	)

	return applicable
}

// policyMatchesCategory checks if a policy applies to the given ad category.
// Policies with category="all" match every category.
func (m *StrategyMatrix) policyMatchesCategory(p types.Policy, category string) bool {
	return p.Category == "all" || p.Category == category
}

// policyMatchesRegion checks if a policy applies to the given region.
//
// Matching rules:
//   - "Global" matches every region
//   - Exact match: "US" matches "US"
//   - Prefix match: "MENA" matches "MENA_SA", "EU" matches "EU_DE"
func (m *StrategyMatrix) policyMatchesRegion(p types.Policy, region string) bool {
	if p.Region == "Global" {
		return true
	}
	if p.Region == region {
		return true
	}
	// Prefix match: policy for "MENA" matches region "MENA_SA"
	if strings.HasPrefix(region, p.Region+"_") {
		return true
	}
	return false
}

// GetRiskLevel returns the risk classification for an ad category.
// Unknown categories default to RiskMedium (fail-safe: not low enough to auto-pass,
// not critical enough to overwhelm the comprehensive pipeline).
func (m *StrategyMatrix) GetRiskLevel(category string) types.RiskLevel {
	if level, ok := m.CategoryRisk[category]; ok {
		return level
	}
	m.logger.Warn("unknown category, defaulting to medium risk",
		slog.String("category", category),
	)
	return types.RiskMedium
}

// GetRegionStrictness returns the regulatory strictness level for a region.
// Reads from the metadata section of region_rules.json — data-driven, not derived.
//
// Unknown regions default to "strict" (fail-closed: assume strictest regulations).
// Prefix matching: "MENA_SA" can fall back to "MENA" metadata if exact match not found.
func (m *StrategyMatrix) GetRegionStrictness(region string) string {
	// Exact match on metadata.
	if meta, ok := m.RegionRules.Metadata[region]; ok {
		return meta.Strictness
	}

	// Prefix match: "EU_DE" → check "EU" metadata.
	for r, meta := range m.RegionRules.Metadata {
		if strings.HasPrefix(region, r+"_") {
			return meta.Strictness
		}
	}

	m.logger.Warn("unknown region, defaulting to strict",
		slog.String("region", region),
	)
	return "strict"
}

// GetRegionCategoryRule returns the specific rule for a (region, category) pair.
// Returns the region-specific rule first, then falls back to Global rules.
// If no rule is found, returns a zero value and false.
func (m *StrategyMatrix) GetRegionCategoryRule(region, category string) (types.RegionCategoryRule, bool) {
	// Try exact region match first
	if categories, ok := m.RegionRules.Rules[region]; ok {
		if rule, ok := categories[category]; ok {
			return rule, true
		}
	}

	// Try prefix match: "MENA_SA" → check parent "MENA"
	for r, categories := range m.RegionRules.Rules {
		if strings.HasPrefix(region, r+"_") {
			if rule, ok := categories[category]; ok {
				return rule, true
			}
		}
	}

	// Fall back to Global
	if categories, ok := m.RegionRules.Rules["Global"]; ok {
		if rule, ok := categories[category]; ok {
			return rule, true
		}
	}

	return types.RegionCategoryRule{}, false
}

// GetReviewPlan determines the review pipeline configuration for a given
// region+category combination. Combines risk level and region strictness
// to select pipeline depth, agent roster, turn limits, and safety thresholds.
//
// Pipeline selection matrix:
//
//	critical risk             → comprehensive + verification
//	strict region (any risk)  → comprehensive + verification
//	high risk                 → standard + verification
//	low risk                  → fast + no auto-reject
//	medium risk (default)     → standard
func (m *StrategyMatrix) GetReviewPlan(region, category string) types.ReviewPlan {
	risk := m.GetRiskLevel(category)
	strictness := m.GetRegionStrictness(region)

	switch {
	case risk == types.RiskCritical:
		return types.ReviewPlan{
			Pipeline:            "comprehensive",
			RequiredAgents:      []string{"content", "policy", "region", "adjudicator"},
			MaxTurns:            10,
			ConfidenceThreshold: 0.85,
			RequireVerification: true,
			AllowAutoReject:     true,
		}
	case strictness == "strict":
		return types.ReviewPlan{
			Pipeline:            "comprehensive",
			RequiredAgents:      []string{"content", "policy", "region", "adjudicator"},
			MaxTurns:            10,
			ConfidenceThreshold: 0.85,
			RequireVerification: true,
			AllowAutoReject:     true,
		}
	case risk == types.RiskHigh:
		return types.ReviewPlan{
			Pipeline:            "standard",
			RequiredAgents:      []string{"content", "policy", "region", "adjudicator"},
			MaxTurns:            6,
			ConfidenceThreshold: 0.75,
			RequireVerification: true,
			AllowAutoReject:     true,
		}
	case risk == types.RiskLow:
		return types.ReviewPlan{
			Pipeline:            "fast",
			RequiredAgents:      []string{"content"},
			MaxTurns:            3,
			ConfidenceThreshold: 0.65,
			RequireVerification: false,
			AllowAutoReject:     false,
		}
	default: // RiskMedium
		return types.ReviewPlan{
			Pipeline:            "standard",
			RequiredAgents:      []string{"content", "policy", "adjudicator"},
			MaxTurns:            6,
			ConfidenceThreshold: 0.70,
			RequireVerification: false,
			AllowAutoReject:     true,
		}
	}
}
