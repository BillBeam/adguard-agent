package agent

import (
	"fmt"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/compact"
	"github.com/BillBeam/adguard-agent/internal/types"
)

const (
	DefaultMaxOutputTokens   = 8192
	EscalatedMaxOutputTokens = 65536
	MaxRecoveryAttempts      = 3
)

// LoopConfig is the complete configuration for an agentic loop run.
// Built from ReviewPlan + AdContent + applicable policies.
type LoopConfig struct {
	// From ReviewPlan.
	MaxTurns            int
	ConfidenceThreshold float64
	AllowAutoReject     bool
	RequireVerification bool
	Pipeline            string // "fast", "standard", "comprehensive"

	// Tools and executor.
	Tools        []types.ToolDefinition
	ToolExecutor ToolExecutor

	// Prompt.
	SystemPrompt string

	// Token control.
	DefaultMaxTokens    int
	EscalatedMaxTokens  int
	MaxRecoveryAttempts int

	// Phase 3: Context Management (nil = disabled).
	ContextManager *compact.ContextManager

	// Phase 3: Token Budget (nil = no limit).
	TokenBudget *compact.TokenBudget

	// Phase 5: Hook system (nil/empty = no hooks, backward compatible).
	PreToolHooks  []PreToolHook
	PostToolHooks []PostToolHook
	StopHooks     []StopHook

	// Model override: if non-empty, overrides the LLM client's default model.
	// Set by ModelRouter based on pipeline and agent role.
	Model string
}

// WithContextManagement attaches Context Manager and Token Budget to the config.
// Builder pattern — does not modify NewLoopConfig signature (backward compatible).
func (c *LoopConfig) WithContextManagement(cm *compact.ContextManager, tb *compact.TokenBudget) *LoopConfig {
	c.ContextManager = cm
	c.TokenBudget = tb
	return c
}

// NewLoopConfig builds a LoopConfig from a ReviewPlan and review context.
func NewLoopConfig(
	plan types.ReviewPlan,
	ad *types.AdContent,
	policies []types.Policy,
	tools []types.ToolDefinition,
	executor ToolExecutor,
) *LoopConfig {
	return &LoopConfig{
		MaxTurns:            plan.MaxTurns,
		ConfidenceThreshold: plan.ConfidenceThreshold,
		AllowAutoReject:     plan.AllowAutoReject,
		RequireVerification: plan.RequireVerification,
		Pipeline:            plan.Pipeline,
		Tools:               tools,
		ToolExecutor:        executor,
		SystemPrompt:        buildSystemPrompt(ad, policies, plan),
		DefaultMaxTokens:    DefaultMaxOutputTokens,
		EscalatedMaxTokens:  EscalatedMaxOutputTokens,
		MaxRecoveryAttempts: MaxRecoveryAttempts,
	}
}

// buildSystemPrompt constructs the system prompt for the review agent.
// Contains: role definition + review pipeline info + ad content + applicable
// policies (with full rule_text for LLM reference) + review instructions + output format.
func buildSystemPrompt(ad *types.AdContent, policies []types.Policy, plan types.ReviewPlan) string {
	var b strings.Builder

	// 1. Role definition.
	b.WriteString("You are an ad content safety review agent for an international advertising platform. ")
	b.WriteString("Your task is to review the following advertisement for policy compliance.\n\n")

	// 2. Review pipeline info.
	fmt.Fprintf(&b, "Review pipeline: %s\n", plan.Pipeline)
	fmt.Fprintf(&b, "Confidence threshold: %.2f (below this → MANUAL_REVIEW)\n", plan.ConfidenceThreshold)
	if !plan.AllowAutoReject {
		b.WriteString("Auto-reject: DISABLED (output MANUAL_REVIEW instead of REJECTED)\n")
	}
	b.WriteString("\n")

	// 3. Ad content (all fields from AdContent).
	b.WriteString("=== AD CONTENT ===\n")
	fmt.Fprintf(&b, "Ad ID: %s\n", ad.ID)
	fmt.Fprintf(&b, "Type: %s\n", ad.Type)
	fmt.Fprintf(&b, "Region: %s\n", ad.Region)
	fmt.Fprintf(&b, "Category: %s\n", ad.Category)
	fmt.Fprintf(&b, "Advertiser: %s\n\n", ad.AdvertiserID)

	b.WriteString("Creative:\n")
	fmt.Fprintf(&b, "  Headline: %s\n", ad.Content.Headline)
	fmt.Fprintf(&b, "  Body: %s\n", ad.Content.Body)
	fmt.Fprintf(&b, "  CTA: %s\n", ad.Content.CTA)
	if ad.Content.ImageDescription != "" {
		fmt.Fprintf(&b, "  Image: %s\n", ad.Content.ImageDescription)
	}
	b.WriteString("\n")

	b.WriteString("Landing Page:\n")
	fmt.Fprintf(&b, "  URL: %s\n", ad.LandingPage.URL)
	fmt.Fprintf(&b, "  Description: %s\n", ad.LandingPage.Description)
	fmt.Fprintf(&b, "  Accessible: %v\n", ad.LandingPage.IsAccessible)
	fmt.Fprintf(&b, "  Mobile Optimized: %v\n\n", ad.LandingPage.IsMobileOptimized)

	// 4. Applicable policies (ID + severity + source + full rule_text).
	b.WriteString("=== APPLICABLE POLICIES ===\n")
	for i, p := range policies {
		fmt.Fprintf(&b, "%d. [%s] severity=%s region=%s category=%s\n", i+1, p.ID, p.Severity, p.Region, p.Category)
		fmt.Fprintf(&b, "   Source: %s\n", p.Source)
		fmt.Fprintf(&b, "   Rule: %s\n\n", p.RuleText)
	}

	// 5. Review instructions.
	b.WriteString("=== INSTRUCTIONS ===\n")
	b.WriteString("1. Use analyze_content to detect problematic claims, misleading language, and Algospeak in the ad text.\n")
	b.WriteString("2. Use match_policies to check which policies are violated based on the detected signals.\n")
	b.WriteString("3. Use check_region_compliance to verify region-specific regulatory requirements.\n")
	b.WriteString("4. Use check_landing_page to verify landing page compliance and ad-content consistency.\n")
	b.WriteString("5. Use lookup_history to check advertiser reputation and similar past cases for consistency.\n")
	b.WriteString("6. After analysis, output your final review decision as JSON.\n\n")

	// 6. Output format.
	b.WriteString("=== OUTPUT FORMAT ===\n")
	b.WriteString("When done, output ONLY a JSON object:\n")
	b.WriteString(`{"decision":"PASSED|REJECTED|MANUAL_REVIEW","confidence":0.0-1.0,`)
	b.WriteString(`"violations":[{"policy_id":"...","severity":"...","description":"...","confidence":0.0-1.0,"evidence":"..."}],`)
	b.WriteString(`"reasoning":"brief explanation"}`)
	b.WriteString("\n")

	return b.String()
}
