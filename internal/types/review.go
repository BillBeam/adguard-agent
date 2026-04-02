package types

import "time"

// ReviewDecision is the outcome of an ad review.
type ReviewDecision string

const (
	DecisionPassed       ReviewDecision = "PASSED"
	DecisionRejected     ReviewDecision = "REJECTED"
	DecisionManualReview ReviewDecision = "MANUAL_REVIEW"
)

// AdContent represents an ad submitted for review.
// Structure matches data/samples/all_samples.json.
type AdContent struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`          // "text", "image_text", "video", "landing_page"
	Region       string      `json:"region"`         // "US", "EU", "MENA_SA", "SEA_ID", "Global", etc.
	Category     string      `json:"category"`       // "healthcare", "alcohol", "ecommerce", etc.
	AdvertiserID string      `json:"advertiser_id"`
	Content      AdBody      `json:"content"`
	LandingPage  LandingPage `json:"landing_page"`
}

// AdBody holds the creative content of an ad.
type AdBody struct {
	Headline         string `json:"headline"`
	Body             string `json:"body"`
	CTA              string `json:"cta"`
	ImageDescription string `json:"image_description,omitempty"` // present for image_text type ads
}

// LandingPage describes the destination page linked from the ad.
type LandingPage struct {
	URL              string `json:"url"`
	Description      string `json:"description"`
	IsAccessible     bool   `json:"is_accessible"`
	IsMobileOptimized bool  `json:"is_mobile_optimized"`
}

// TestAdSample extends AdContent with expected test outcome for validation.
type TestAdSample struct {
	AdContent
	ExpectedResult  string   `json:"expected_result"`  // "PASSED", "REJECTED", "MANUAL_REVIEW"
	ExpectedReasons []string `json:"expected_reasons"`
}

// ReviewResult captures the full output of an ad review.
type ReviewResult struct {
	AdID           string           `json:"ad_id"`
	Decision       ReviewDecision   `json:"decision"`
	Confidence     float64          `json:"confidence"`      // 0.0 to 1.0
	Violations     []PolicyViolation `json:"violations"`
	RiskLevel      RiskLevel        `json:"risk_level"`
	AgentTrace     []string         `json:"agent_trace"`     // ordered list of agent actions
	ReviewDuration time.Duration    `json:"review_duration"`
	Timestamp      time.Time        `json:"timestamp"`
}

// PolicyViolation records a single policy breach found during review.
type PolicyViolation struct {
	PolicyID    string  `json:"policy_id"`
	Severity    string  `json:"severity"`
	Description string  `json:"description"`
	Confidence  float64 `json:"confidence"` // 0.0 to 1.0
	Evidence    string  `json:"evidence"`   // extracted text or signal triggering the violation
}

// ReviewPlan defines the review pipeline configuration determined by
// the strategy matrix based on (region, category) risk assessment.
type ReviewPlan struct {
	Pipeline            string   `json:"pipeline"`              // "fast", "standard", "comprehensive"
	RequiredAgents      []string `json:"required_agents"`
	MaxTurns            int      `json:"max_turns"`
	ConfidenceThreshold float64  `json:"confidence_threshold"`  // below this → MANUAL_REVIEW
	RequireVerification bool     `json:"require_verification"`  // force L4 verification re-check
	AllowAutoReject     bool     `json:"allow_auto_reject"`     // false for low-risk categories
}
