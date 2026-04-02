package strategy

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// loadPolicies reads and validates the policy knowledge base from a JSON file.
// Returns a map keyed by policy ID for O(1) lookup.
func loadPolicies(path string) (map[string]types.Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading policy file: %w", err)
	}

	var policies []types.Policy
	if err := json.Unmarshal(data, &policies); err != nil {
		return nil, fmt.Errorf("parsing policy JSON: %w", err)
	}

	m := make(map[string]types.Policy, len(policies))
	for i, p := range policies {
		if p.ID == "" {
			return nil, fmt.Errorf("policy at index %d has empty ID", i)
		}
		if _, dup := m[p.ID]; dup {
			return nil, fmt.Errorf("duplicate policy ID: %s", p.ID)
		}
		if err := validatePolicy(p); err != nil {
			return nil, fmt.Errorf("invalid policy %s: %w", p.ID, err)
		}
		m[p.ID] = p
	}
	return m, nil
}

// loadRegionRules reads the region rules JSON file.
// The file has structure: {"rules": {"US": {"alcohol": {...}, ...}, ...}}
func loadRegionRules(path string) (*types.RegionRules, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading region rules file: %w", err)
	}

	var rules types.RegionRules
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("parsing region rules JSON: %w", err)
	}

	if len(rules.Rules) == 0 {
		return nil, fmt.Errorf("region rules file contains no regions")
	}

	for region, categories := range rules.Rules {
		for category, rule := range categories {
			if err := validateRegionCategoryRule(region, category, rule); err != nil {
				return nil, fmt.Errorf("invalid rule %s/%s: %w", region, category, err)
			}
		}
	}
	return &rules, nil
}

// loadCategoryRisk reads the category-to-risk-level mapping from a JSON file.
// This is a flat map: {"gambling": "critical", "ecommerce": "low", ...}
func loadCategoryRisk(path string) (map[string]types.RiskLevel, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading category risk file: %w", err)
	}

	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing category risk JSON: %w", err)
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("category risk file is empty")
	}

	m := make(map[string]types.RiskLevel, len(raw))
	for cat, level := range raw {
		rl := types.RiskLevel(level)
		if !types.ValidRiskLevels[rl] {
			return nil, fmt.Errorf("invalid risk level %q for category %q", level, cat)
		}
		m[cat] = rl
	}
	return m, nil
}

// validatePolicy checks that a policy has required fields with valid values.
func validatePolicy(p types.Policy) error {
	validSeverities := map[string]bool{
		"critical": true, "high": true, "moderate": true, "low": true,
	}
	if !validSeverities[p.Severity] {
		return fmt.Errorf("invalid severity %q", p.Severity)
	}
	if p.Region == "" {
		return fmt.Errorf("region must not be empty")
	}
	if p.Category == "" {
		return fmt.Errorf("category must not be empty")
	}
	if p.RuleText == "" {
		return fmt.Errorf("rule_text must not be empty")
	}
	if p.Source == "" {
		return fmt.Errorf("source must not be empty")
	}
	return nil
}

// validateRegionCategoryRule checks that a region category rule has valid status.
func validateRegionCategoryRule(region, category string, r types.RegionCategoryRule) error {
	validStatuses := map[string]bool{
		"prohibited": true, "restricted": true, "permitted": true,
		"prohibited_brand_content": true, "varies": true,
	}
	if !validStatuses[r.Status] {
		return fmt.Errorf("invalid status %q", r.Status)
	}
	return nil
}
