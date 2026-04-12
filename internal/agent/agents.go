package agent

import (
	"fmt"
	"strings"
	"sync"

	"github.com/BillBeam/adguard-agent/internal/store"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// Agent role definitions for the Multi-Agent review system.
//
// 4 Agent roles aligned with the Perception-Attribution-Adjudication (感知-归因-研判) pipeline:
//   - ContentAgent  (Perception+Attribution): deep content analysis + policy matching
//   - PolicyAgent   (Attribution+Adjudication): policy compliance from strategy platform perspective
//   - RegionAgent   (Adjudication):      region-specific regulatory compliance
//   - AdjudicatorAgent (core Adjudication): conflict detection + weighted arbitration + L3 False-positive control (误伤控制)

// AgentRole identifies a specialist agent type.
type AgentRole string

const (
	RoleContent     AgentRole = "content"
	RolePolicy      AgentRole = "policy"
	RoleRegion      AgentRole = "region"
	RoleAdjudicator AgentRole = "adjudicator"
	RoleAppeal      AgentRole = "appeal"  // Phase 5: appeal re-review
	RoleSingle      AgentRole = "single"  // Phase 2 single-agent path (fast pipeline)
)

// SystemPromptDynamicBoundary separates the static (shared, cacheable) prefix
// from the per-review dynamic suffix in system prompts.
const SystemPromptDynamicBoundary = "\n--- DYNAMIC CONTEXT ---\n"

// AgentSpec defines a specialist agent's configuration.
type AgentSpec struct {
	Role     AgentRole
	Tools    []string // tool names this agent can use (empty = no tools)
	MaxTurns int
}

// SpecialistAgentSpecs returns the specs for the 3 specialist agents.
// MaxTurns vary by pipeline: standard gets base, comprehensive gets extended.
func SpecialistAgentSpecs(pipeline string) []AgentSpec {
	turnMultiplier := 4
	if pipeline == "comprehensive" {
		turnMultiplier = 6
	}
	return []AgentSpec{
		{Role: RoleContent, Tools: []string{"analyze_content", "check_landing_page"}, MaxTurns: turnMultiplier},
		{Role: RolePolicy, Tools: []string{"match_policies", "query_policy_kb"}, MaxTurns: turnMultiplier},
		{Role: RoleRegion, Tools: []string{"check_region_compliance", "query_policy_kb", "lookup_history"}, MaxTurns: turnMultiplier},
	}
}

// AdjudicatorSpec returns the spec for the Adjudicator agent.
func AdjudicatorSpec(pipeline string) AgentSpec {
	maxTurns := 2
	if pipeline == "comprehensive" {
		maxTurns = 3
	}
	return AgentSpec{Role: RoleAdjudicator, Tools: nil, MaxTurns: maxTurns}
}

// --- Static/Dynamic System Prompt Split ---

var (
	staticPrefixOnce sync.Once
	staticPrefix     string
)

// buildStaticPromptPrefix returns the shared, cacheable prefix for all specialist agents.
// Computed once per process via sync.Once.
func buildStaticPromptPrefix() string {
	staticPrefixOnce.Do(func() {
		var b strings.Builder

		// 1. Role preamble.
		b.WriteString("You are a specialist agent in a multi-agent ad content safety review system ")
		b.WriteString("for an international advertising platform. Your task is to review advertisements ")
		b.WriteString("for policy compliance.\n\n")

		// 2. Review methodology.
		b.WriteString("=== REVIEW METHODOLOGY ===\n")
		b.WriteString("The review follows the Perception-Attribution-Adjudication (感知-归因-研判) pipeline:\n")
		b.WriteString("- Perception (感知): extract violation signals from ad content (claims, Algospeak, landing page issues)\n")
		b.WriteString("- Attribution (归因): map detected signals to specific policy violations\n")
		b.WriteString("- Adjudication (研判): assess severity, check regional requirements, and make the final decision\n\n")

		// 3. Output format (shared by all specialist agents).
		b.WriteString("=== OUTPUT FORMAT ===\n")
		b.WriteString("When done, output ONLY a JSON object:\n")
		b.WriteString(`{"decision":"PASSED|REJECTED|MANUAL_REVIEW","confidence":0.0-1.0,`)
		b.WriteString(`"violations":[{"policy_id":"...","severity":"...","description":"...","confidence":0.0-1.0,"evidence":"..."}],`)
		b.WriteString(`"reasoning":"brief explanation"}`)
		b.WriteString("\n\n")

		// 4. Tool usage principles.
		b.WriteString("=== TOOL USAGE PRINCIPLES ===\n")
		b.WriteString("- Always use available tools to gather evidence before making judgments.\n")
		b.WriteString("- Fail-closed: when uncertain about compliance, output MANUAL_REVIEW.\n")
		b.WriteString("- Each violation must cite specific evidence from the ad content.\n")
		b.WriteString("- Use query_policy_kb to look up detailed policy text before making compliance judgments.\n\n")

		// 5. Algospeak detection guidance.
		b.WriteString("=== ALGOSPEAK DETECTION ===\n")
		b.WriteString("Watch for Algospeak — intentional misspellings, phonetic substitutions, Unicode tricks, ")
		b.WriteString("and emoji encoding used to evade content filters:\n")
		b.WriteString("- Phonetic: 'wei減' for weight loss, 'c@nn@bis' for cannabis\n")
		b.WriteString("- Unicode: fullwidth characters, Cyrillic lookalikes, invisible characters\n")
		b.WriteString("- Emoji: 🍃 for marijuana, 💊 for pills, 🎰 for gambling\n")
		b.WriteString("- Abbreviations: 'CBD' disguised as wellness, 'BTC' in financial contexts\n")

		staticPrefix = b.String()
	})
	return staticPrefix
}

// BuildAgentSystemPrompt constructs the system prompt for a specialist agent.
// Uses static prefix (shared, cacheable) + dynamic suffix (per-agent, per-review).
func BuildAgentSystemPrompt(role AgentRole, ad *types.AdContent, policies []types.Policy, plan types.ReviewPlan, memorySection string) string {
	var b strings.Builder
	b.WriteString(buildStaticPromptPrefix())
	b.WriteString(SystemPromptDynamicBoundary)
	buildDynamicSuffix(&b, role, ad, policies, plan, memorySection)
	return b.String()
}

// buildDynamicSuffix writes the per-agent, per-review dynamic section.
func buildDynamicSuffix(b *strings.Builder, role AgentRole, ad *types.AdContent, policies []types.Policy, plan types.ReviewPlan, memorySection string) {
	// 1. Role-specific description.
	writeRoleDescription(b, role)

	// 2. Pipeline parameters.
	fmt.Fprintf(b, "\nReview pipeline: %s\n", plan.Pipeline)
	fmt.Fprintf(b, "Confidence threshold: %.2f\n\n", plan.ConfidenceThreshold)

	// 3. Role-specific tool instructions.
	writeRoleToolInstructions(b, role)

	// 4. Ad content.
	writeAdContent(b, ad)

	// 5. Policies.
	writePolicies(b, policies)

	// 6. Agent memory (empty until wired).
	if memorySection != "" {
		b.WriteString("\n")
		b.WriteString(memorySection)
	}
}

func writeRoleDescription(b *strings.Builder, role AgentRole) {
	b.WriteString("=== YOUR ROLE ===\n")
	switch role {
	case RoleContent:
		b.WriteString("Perception+Attribution (感知+归因): You are an ad content safety analysis specialist. ")
		b.WriteString("Your focus is detecting problematic claims, misleading language, Algospeak, ")
		b.WriteString("and false regulatory claims in ad creatives.\n")
	case RolePolicy:
		b.WriteString("Attribution+Adjudication (归因+研判): You are an ad policy compliance specialist. ")
		b.WriteString("Your focus is determining whether ads meet all applicable policy requirements ")
		b.WriteString("based on the advertising policy framework and regional regulations.\n")
	case RoleRegion:
		b.WriteString("Adjudication (研判): You are an international ad market compliance specialist. ")
		b.WriteString("Your focus is region-specific category restrictions, landing page compliance, ")
		b.WriteString("advertiser history, and cultural sensitivity.\n")
	}
}

func writeRoleToolInstructions(b *strings.Builder, role AgentRole) {
	b.WriteString("=== YOUR TASK ===\n")
	switch role {
	case RoleContent:
		b.WriteString("1. Use analyze_content to detect content signals (claims, Algospeak, misleading text).\n")
		b.WriteString("2. Use check_landing_page to verify landing page compliance and ad-content consistency.\n")
		b.WriteString("3. Output your analysis as JSON.\n\n")
	case RolePolicy:
		b.WriteString("1. Use match_policies to identify applicable policy violations for this region+category.\n")
		b.WriteString("2. Use query_policy_kb to look up detailed policy text when you need full rule specifications.\n")
		b.WriteString("3. Focus on: category admission, required qualifications, mandatory disclosures.\n")
		b.WriteString("4. Output your analysis as JSON.\n\n")
	case RoleRegion:
		b.WriteString("1. Use check_region_compliance to check if this category is prohibited/restricted in the target region.\n")
		b.WriteString("2. Use query_policy_kb to look up region-specific policy details when needed.\n")
		b.WriteString("3. Use lookup_history to check the advertiser's compliance record and similar past cases.\n")
		b.WriteString("4. For MENA regions: pay special attention to Sharia compliance and cultural sensitivity.\n")
		b.WriteString("5. Output your analysis as JSON.\n\n")
	}
}

// --- Adjudicator and Appeal prompts (unchanged — different structure) ---

// BuildAdjudicatorPrompt constructs the Adjudicator's system prompt
// with the specialist agents' results embedded.
func BuildAdjudicatorPrompt(ad *types.AdContent, agentResults []AgentResult, plan types.ReviewPlan) string {
	var b strings.Builder

	b.WriteString("You are the final decision-maker in an ad content safety review system. ")
	b.WriteString("Multiple specialist agents have independently analyzed the same advertisement. ")
	b.WriteString("Your task is to synthesize their findings into a single, authoritative review decision.\n\n")

	// Pipeline info.
	fmt.Fprintf(&b, "Review pipeline: %s\n", plan.Pipeline)
	fmt.Fprintf(&b, "Confidence threshold: %.2f\n\n", plan.ConfidenceThreshold)

	// Ad summary.
	b.WriteString("=== AD SUMMARY ===\n")
	fmt.Fprintf(&b, "Ad ID: %s | Region: %s | Category: %s | Advertiser: %s\n",
		ad.ID, ad.Region, ad.Category, ad.AdvertiserID)
	fmt.Fprintf(&b, "Headline: %s\n\n", ad.Content.Headline)

	// Agent results.
	b.WriteString("=== SPECIALIST AGENT REPORTS ===\n\n")
	for _, ar := range agentResults {
		fmt.Fprintf(&b, "--- %s Agent ---\n", strings.ToUpper(string(ar.Role)))
		fmt.Fprintf(&b, "Decision: %s (confidence: %.2f)\n", ar.Decision, ar.Confidence)
		if len(ar.Violations) > 0 {
			b.WriteString("Violations:\n")
			for _, v := range ar.Violations {
				fmt.Fprintf(&b, "  - [%s] %s (severity=%s, confidence=%.2f)\n",
					v.PolicyID, v.Description, v.Severity, v.Confidence)
			}
		}
		if ar.Reasoning != "" {
			fmt.Fprintf(&b, "Reasoning: %s\n", ar.Reasoning)
		}
		b.WriteString("\n")
	}

	// Instructions.
	b.WriteString("=== INSTRUCTIONS ===\n")
	b.WriteString("1. Identify conflicts: Which agents disagree? On what issues?\n")
	b.WriteString("2. Apply weighted evaluation:\n")
	b.WriteString("   - Content Agent has highest authority on text/media content issues\n")
	b.WriteString("   - Policy Agent has highest authority on policy compliance issues\n")
	b.WriteString("   - Region Agent has highest authority on regional regulatory issues\n")
	b.WriteString("3. Apply false-positive control (L3):\n")
	b.WriteString("   - If ALL agents agree → high confidence, maintain decision\n")
	b.WriteString("   - If 2:1 split → follow majority, reduce confidence\n")
	b.WriteString("   - If 3-way disagreement → MANUAL_REVIEW (case too complex for automation)\n")
	b.WriteString("   - If ANY agent found critical-severity violation → at minimum MANUAL_REVIEW\n")
	b.WriteString("4. Output your final decision as JSON.\n\n")

	// Output format.
	b.WriteString("=== OUTPUT FORMAT ===\n")
	b.WriteString("Output ONLY a JSON object:\n")
	b.WriteString(`{"decision":"PASSED|REJECTED|MANUAL_REVIEW","confidence":0.0-1.0,`)
	b.WriteString(`"violations":[...],"conflicts":[{"agents":["a","b"],"issue":"..."}],`)
	b.WriteString(`"agent_decisions":{"content":"...","policy":"...","region":"..."},`)
	b.WriteString(`"reasoning":"brief explanation"}`)
	b.WriteString("\n")

	return b.String()
}

// --- Shared prompt helpers ---

func writeAdContent(b *strings.Builder, ad *types.AdContent) {
	b.WriteString("=== AD CONTENT ===\n")
	fmt.Fprintf(b, "Ad ID: %s | Type: %s | Region: %s | Category: %s | Advertiser: %s\n\n",
		ad.ID, ad.Type, ad.Region, ad.Category, ad.AdvertiserID)
	fmt.Fprintf(b, "Headline: %s\n", ad.Content.Headline)
	fmt.Fprintf(b, "Body: %s\n", ad.Content.Body)
	if ad.Content.CTA != "" {
		fmt.Fprintf(b, "CTA: %s\n", ad.Content.CTA)
	}
	if ad.Content.ImageDescription != "" {
		fmt.Fprintf(b, "Image: %s\n", ad.Content.ImageDescription)
	}
	fmt.Fprintf(b, "\nLanding Page: %s\n", ad.LandingPage.URL)
	fmt.Fprintf(b, "Description: %s\n", ad.LandingPage.Description)
	fmt.Fprintf(b, "Accessible: %v | Mobile Optimized: %v\n\n", ad.LandingPage.IsAccessible, ad.LandingPage.IsMobileOptimized)
}

func writePolicies(b *strings.Builder, policies []types.Policy) {
	b.WriteString("=== APPLICABLE POLICIES (summary) ===\n")
	b.WriteString("Use query_policy_kb tool to look up detailed policy text when needed.\n\n")
	for i, p := range policies {
		summary := truncatePolicySummary(p.RuleText, 80)
		fmt.Fprintf(b, "%d. [%s] severity=%s region=%s category=%s — %s\n",
			i+1, p.ID, p.Severity, p.Region, p.Category, summary)
	}
	b.WriteString("\n")
}

// truncatePolicySummary extracts the first sentence or the first maxLen characters of the policy text.
func truncatePolicySummary(text string, maxLen int) string {
	// Take the first sentence.
	if idx := strings.Index(text, ". "); idx >= 0 && idx < maxLen {
		return text[:idx+1]
	}
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

// BuildAppealSystemPrompt constructs the prompt for the Appeal Agent.
// Sees: ad content + original decision + violations + appeal reason.
// Does NOT see: agent_trace (independence from original review).
func BuildAppealSystemPrompt(ad *types.AdContent, record *store.ReviewRecord, appealReason string) string {
	var b strings.Builder

	b.WriteString("You are an independent appeal reviewer for an ad content safety system. ")
	b.WriteString("An advertiser's ad was REJECTED and they have submitted an appeal. ")
	b.WriteString("Your task is to determine whether the original REJECTED decision should be ")
	b.WriteString("upheld, overturned (change to PASSED), or partially modified.\n\n")

	writeAdContent(&b, ad)

	b.WriteString("=== ORIGINAL REVIEW DECISION ===\n")
	fmt.Fprintf(&b, "Decision: %s\n", record.Decision)
	fmt.Fprintf(&b, "Confidence: %.2f\n", record.Confidence)
	if len(record.Violations) > 0 {
		b.WriteString("Violations:\n")
		for _, v := range record.Violations {
			fmt.Fprintf(&b, "  - [%s] %s (severity=%s)\n", v.PolicyID, v.Description, v.Severity)
		}
	}
	b.WriteString("\n")

	b.WriteString("=== ADVERTISER'S APPEAL REASON ===\n")
	fmt.Fprintf(&b, "%s\n\n", appealReason)

	b.WriteString("=== INSTRUCTIONS ===\n")
	b.WriteString("Consider the advertiser's appeal reason carefully. Determine whether:\n")
	b.WriteString("- The original violations are valid and the REJECTED decision should be UPHELD\n")
	b.WriteString("- The advertiser's explanation shows the ad is compliant and should be OVERTURNED (PASSED)\n")
	b.WriteString("- Some violations are valid but others are not (MANUAL_REVIEW for partial modification)\n\n")

	b.WriteString("=== OUTPUT FORMAT ===\n")
	b.WriteString("Output ONLY a JSON object:\n")
	b.WriteString(`{"decision":"PASSED|REJECTED|MANUAL_REVIEW","confidence":0.0-1.0,`)
	b.WriteString(`"violations":[],"reasoning":"explanation of your appeal decision"}`)
	b.WriteString("\n")

	return b.String()
}
