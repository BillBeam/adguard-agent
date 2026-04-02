package types

import "time"

// RiskLevel classifies ad categories by regulatory and content risk.
// Drives the orchestration engine's pipeline selection.
type RiskLevel string

const (
	RiskCritical RiskLevel = "critical" // gambling, crypto, weapons, drugs, adult_content
	RiskHigh     RiskLevel = "high"     // healthcare, finance, alcohol, tobacco, political
	RiskMedium   RiskLevel = "medium"   // weight_loss, beauty, dating, supplement
	RiskLow      RiskLevel = "low"      // ecommerce, app_promotion, education
)

// ValidRiskLevels enumerates accepted values for validation during data loading.
var ValidRiskLevels = map[RiskLevel]bool{
	RiskCritical: true,
	RiskHigh:     true,
	RiskMedium:   true,
	RiskLow:      true,
}

// Policy represents a single ad review policy from the policy knowledge base.
// Loaded from data/policy_kb.json. The LLM agents reference rule_text directly
// when explaining review decisions.
type Policy struct {
	ID       string `json:"id"`
	Region   string `json:"region"`    // "Global", "US", "EU", "MENA_SA", etc.
	Category string `json:"category"`  // ad category: "healthcare", "alcohol", "all", etc.
	RuleText string `json:"rule_text"` // full policy text — consumed by LLM agents during review
	Severity string `json:"severity"`  // "critical", "high", "moderate", "low"
	Source   string `json:"source"`    // policy origin, e.g. "TikTok Advertising Policies - ..."
}

// RegionCategoryRule defines the status and requirements for a specific
// (region, category) combination. Loaded from data/region_rules.json.
type RegionCategoryRule struct {
	Status       string   `json:"status"`                 // "prohibited", "restricted", "permitted", "prohibited_brand_content", "varies"
	MinAge       int      `json:"min_age,omitempty"`      // minimum age for targeting, e.g. 18, 21, 25
	Requirements []string `json:"requirements,omitempty"` // specific compliance requirements
	Notes        string   `json:"notes,omitempty"`        // human-readable context
}

// RegionMetadata holds per-region metadata such as strictness level.
type RegionMetadata struct {
	Strictness  string `json:"strictness"`  // "strict", "standard"
	Description string `json:"description"`
}

// RegionRules holds all category rules and metadata for all regions.
// JSON structure: {"metadata": {"US": {...}}, "rules": {"US": {"alcohol": {...}, ...}, ...}}
type RegionRules struct {
	Metadata map[string]RegionMetadata                `json:"metadata"`
	Rules    map[string]map[string]RegionCategoryRule `json:"rules"`
}

// AdvertiserReputation models an advertiser's trust profile.
// Used by the orchestration engine for pipeline selection (high-trust advertisers
// may qualify for fast-track review) and by the false-positive control system (L2).
type AdvertiserReputation struct {
	AdvertiserID        string  `json:"advertiser_id"`
	TrustScore          float64 `json:"trust_score"`           // 0.0 to 1.0
	HistoricalViolations int    `json:"historical_violations"` // count of past violations
	AppealSuccessRate   float64 `json:"appeal_success_rate"`   // 0.0 to 1.0
	RiskCategory        string  `json:"risk_category"`         // "trusted", "standard", "flagged", "probation"
}

// StrategyVersion tracks a specific version of the strategy configuration.
// Supports canary/rollback for strategy updates (Phase 5).
type StrategyVersion struct {
	VersionID  string           `json:"version_id"`
	Status     string           `json:"status"`      // "draft", "canary", "active", "rollback"
	TrafficPct int              `json:"traffic_pct"`  // 0-100, percentage of traffic using this version
	CreatedAt  time.Time        `json:"created_at"`
	Metrics    *StrategyMetrics `json:"metrics,omitempty"`
}

// StrategyMetrics captures the performance of a strategy version.
type StrategyMetrics struct {
	Accuracy          float64 `json:"accuracy"`
	Recall            float64 `json:"recall"`
	FalsePositiveRate float64 `json:"false_positive_rate"`
}
