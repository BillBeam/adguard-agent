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

	// FallbackModel: the degraded model to use when consecutive 529 errors occur.
	// Set by ModelRouter.GetFallback(). Empty = no automatic fallback.
	FallbackModel string

	// EnableStreaming: when true, uses streaming API calls with
	// StreamingToolExecutor for concurrent tool dispatch during LLM response.
	// Falls back to non-streaming on stream errors.
	EnableStreaming bool
}

// WithContextManagement attaches Context Manager and Token Budget to the config.
// Builder pattern — does not modify NewLoopConfig signature (backward compatible).
func (c *LoopConfig) WithContextManagement(cm *compact.ContextManager, tb *compact.TokenBudget) *LoopConfig {
	c.ContextManager = cm
	c.TokenBudget = tb
	return c
}

// NewLoopConfig builds a LoopConfig from a ReviewPlan and review context.
// memorySection is injected into the system prompt dynamic suffix (empty = no memory).
func NewLoopConfig(
	plan types.ReviewPlan,
	ad *types.AdContent,
	policies []types.Policy,
	tools []types.ToolDefinition,
	executor ToolExecutor,
	memorySection string,
) *LoopConfig {
	return &LoopConfig{
		MaxTurns:            plan.MaxTurns,
		ConfidenceThreshold: plan.ConfidenceThreshold,
		AllowAutoReject:     plan.AllowAutoReject,
		RequireVerification: plan.RequireVerification,
		Pipeline:            plan.Pipeline,
		Tools:               tools,
		ToolExecutor:        executor,
		SystemPrompt:        buildSystemPrompt(ad, policies, plan, memorySection),
		DefaultMaxTokens:    DefaultMaxOutputTokens,
		EscalatedMaxTokens:  EscalatedMaxOutputTokens,
		MaxRecoveryAttempts: MaxRecoveryAttempts,
	}
}

// buildSystemPrompt constructs the system prompt for the single-agent review path.
// Uses static prefix (shared with multi-agent) + dynamic suffix (single-agent specific).
func buildSystemPrompt(ad *types.AdContent, policies []types.Policy, plan types.ReviewPlan, memorySection string) string {
	var b strings.Builder

	b.WriteString(buildStaticPromptPrefix())
	b.WriteString(SystemPromptDynamicBoundary)

	// Single-agent role.
	b.WriteString("=== YOUR ROLE ===\n")
	b.WriteString("You are the sole review agent (fast pipeline). You handle all aspects: ")
	b.WriteString("content analysis, policy compliance, and regional regulatory checks.\n\n")

	// Pipeline parameters.
	fmt.Fprintf(&b, "Review pipeline: %s\n", plan.Pipeline)
	fmt.Fprintf(&b, "Confidence threshold: %.2f (below this → MANUAL_REVIEW)\n", plan.ConfidenceThreshold)
	if !plan.AllowAutoReject {
		b.WriteString("Auto-reject: DISABLED (output MANUAL_REVIEW instead of REJECTED)\n")
	}
	b.WriteString("\n")

	// All tool instructions.
	b.WriteString("=== INSTRUCTIONS ===\n")
	b.WriteString("1. Use analyze_content to detect problematic claims, misleading language, and Algospeak.\n")
	b.WriteString("2. Use match_policies to check which policies are violated.\n")
	b.WriteString("3. Use check_region_compliance to verify region-specific requirements.\n")
	b.WriteString("4. Use check_landing_page to verify landing page compliance.\n")
	b.WriteString("5. Use lookup_history to check advertiser reputation and similar past cases.\n")
	b.WriteString("6. Use query_policy_kb for detailed policy text when needed.\n")
	b.WriteString("7. After analysis, output your final review decision as JSON.\n\n")

	// Ad content + policies (shared helpers from agents.go).
	writeAdContent(&b, ad)
	writePolicies(&b, policies)

	// Agent memory.
	if memorySection != "" {
		b.WriteString("\n")
		b.WriteString(memorySection)
	}

	return b.String()
}
