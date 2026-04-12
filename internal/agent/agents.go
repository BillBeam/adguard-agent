package agent

import (
	"fmt"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/store"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// Agent role definitions for the Multi-Agent review system.
//
// 4 Agent roles aligned with "感知-归因-研判" pipeline:
//   - ContentAgent  (感知+归因): deep content analysis + policy matching
//   - PolicyAgent   (归因+研判): policy compliance from strategy platform perspective
//   - RegionAgent   (研判):      region-specific regulatory compliance
//   - AdjudicatorAgent (研判核心): conflict detection + weighted arbitration + L3 false-positive control

// AgentRole identifies a specialist agent type.
type AgentRole string

const (
	RoleContent     AgentRole = "content"
	RolePolicy      AgentRole = "policy"
	RoleRegion      AgentRole = "region"
	RoleAdjudicator AgentRole = "adjudicator"
	RoleAppeal      AgentRole = "appeal"      // Phase 5: appeal re-review
	RoleSingle      AgentRole = "single"      // Phase 2 single-agent path (fast pipeline)
)

// AgentSpec defines a specialist agent's configuration.
type AgentSpec struct {
	Role      AgentRole
	Tools     []string // tool names this agent can use (empty = no tools)
	MaxTurns  int
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

// BuildAgentSystemPrompt constructs the system prompt for a specialist agent.
// Each agent sees the full ad content but has a focused analysis mandate.
func BuildAgentSystemPrompt(role AgentRole, ad *types.AdContent, policies []types.Policy, plan types.ReviewPlan) string {
	var b strings.Builder

	switch role {
	case RoleContent:
		buildContentAgentPrompt(&b, ad, policies, plan)
	case RolePolicy:
		buildPolicyAgentPrompt(&b, ad, policies, plan)
	case RoleRegion:
		buildRegionAgentPrompt(&b, ad, policies, plan)
	case RoleAdjudicator:
		// Adjudicator prompt is built separately with agent results.
	}

	return b.String()
}

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

// --- Specialist agent prompts ---

func buildContentAgentPrompt(b *strings.Builder, ad *types.AdContent, policies []types.Policy, plan types.ReviewPlan) {
	// 感知+归因：content analysis specialist.
	b.WriteString("You are an ad content safety analysis specialist. ")
	b.WriteString("Your focus is detecting problematic claims, misleading language, Algospeak, ")
	b.WriteString("and false regulatory claims in the ad creative.\n\n")

	writeAdContent(b, ad)
	writePolicies(b, policies)

	b.WriteString("=== YOUR TASK ===\n")
	b.WriteString("1. Use analyze_content to detect content signals (claims, Algospeak, misleading text).\n")
	b.WriteString("2. Use check_landing_page to verify landing page compliance and ad-content consistency.\n")
	b.WriteString("3. Output your analysis as JSON.\n\n")

	writeAgentOutputFormat(b)
}

func buildPolicyAgentPrompt(b *strings.Builder, ad *types.AdContent, policies []types.Policy, plan types.ReviewPlan) {
	// 归因+研判：policy compliance specialist.
	b.WriteString("You are an ad policy compliance specialist. ")
	b.WriteString("Your focus is determining whether this ad meets all applicable policy requirements ")
	b.WriteString("based on the TikTok advertising policy framework and regional regulations.\n\n")

	writeAdContent(b, ad)
	writePolicies(b, policies)

	b.WriteString("=== YOUR TASK ===\n")
	b.WriteString("1. Use match_policies to identify applicable policy violations for this region+category.\n")
	b.WriteString("2. Use query_policy_kb to look up detailed policy text when you need full rule specifications.\n")
	b.WriteString("3. Focus on: category admission, required qualifications, mandatory disclosures.\n")
	b.WriteString("4. Output your analysis as JSON.\n\n")

	writeAgentOutputFormat(b)
}

func buildRegionAgentPrompt(b *strings.Builder, ad *types.AdContent, policies []types.Policy, plan types.ReviewPlan) {
	// 研判：region compliance specialist.
	b.WriteString("You are an international ad market compliance specialist. ")
	b.WriteString("Your focus is checking whether this ad is suitable for the target region: ")
	b.WriteString("category restrictions, landing page compliance, advertiser history, cultural sensitivity.\n\n")

	writeAdContent(b, ad)
	writePolicies(b, policies)

	b.WriteString("=== YOUR TASK ===\n")
	b.WriteString("1. Use check_region_compliance to check if this category is prohibited/restricted in the target region.\n")
	b.WriteString("2. Use query_policy_kb to look up region-specific policy details when needed.\n")
	b.WriteString("3. Use lookup_history to check the advertiser's compliance record and similar past cases.\n")
	b.WriteString("4. For MENA regions: pay special attention to Sharia compliance and cultural sensitivity.\n")
	b.WriteString("5. Output your analysis as JSON.\n\n")

	writeAgentOutputFormat(b)
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

// truncatePolicySummary 截取策略文本的第一句或前 maxLen 个字符。
func truncatePolicySummary(text string, maxLen int) string {
	// 取第一句话。
	if idx := strings.Index(text, ". "); idx >= 0 && idx < maxLen {
		return text[:idx+1]
	}
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

func writeAgentOutputFormat(b *strings.Builder) {
	b.WriteString("=== OUTPUT FORMAT ===\n")
	b.WriteString("When done, output ONLY a JSON object:\n")
	b.WriteString(`{"decision":"PASSED|REJECTED|MANUAL_REVIEW","confidence":0.0-1.0,`)
	b.WriteString(`"violations":[{"policy_id":"...","severity":"...","description":"...","confidence":0.0-1.0,"evidence":"..."}],`)
	b.WriteString(`"reasoning":"brief explanation"}`)
	b.WriteString("\n")
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
