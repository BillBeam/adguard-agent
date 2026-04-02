package compact

import (
	"regexp"
	"strings"
)

// Compact prompt design:
//   - NO_TOOLS_PREAMBLE: prevent LLM from calling tools during summarization
//   - <analysis> + <summary> two-phase structure: analysis is scratch pad, summary is output
//   - 6 dimensions tailored for ad review context preservation
//   - FormatCompactSummary(): extract <summary>, discard <analysis>

// BuildCompactPrompt constructs the LLM summarization prompt for AutoCompact.
//
// Ad review 6 dimensions for context preservation:
//  1. Reviewed ads list with decisions (AdID, Decision, Confidence)
//  2. Violation patterns and trends (frequently triggered policies)
//  3. False positive cases and analysis (REJECTED → MANUAL_REVIEW downgrades)
//  4. Regional compliance notes (prohibited/restricted rules)
//  5. Advertiser reputation changes (reputation_category shifts)
//  6. Pending review tasks (remaining ads in batch queue)
func BuildCompactPrompt() string {
	var b strings.Builder

	// NO_TOOLS_PREAMBLE — critical: prevent tool calls during summarization.
	b.WriteString("CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\n")
	b.WriteString("- Do NOT use analyze_content, match_policies, check_region_compliance, ")
	b.WriteString("check_landing_page, lookup_history, or ANY other tool.\n")
	b.WriteString("- You already have all the context you need in the conversation above.\n")
	b.WriteString("- Tool calls will be REJECTED. Your entire response must be plain text: ")
	b.WriteString("an <analysis> block followed by a <summary> block.\n\n")

	// Analysis instruction.
	b.WriteString("Before providing your final summary, wrap your analysis in <analysis> tags ")
	b.WriteString("to organize your thoughts:\n\n")
	b.WriteString("1. Chronologically review each ad that was processed:\n")
	b.WriteString("   - The ad ID, region, category, and advertiser\n")
	b.WriteString("   - The review decision (PASSED/REJECTED/MANUAL_REVIEW) and confidence\n")
	b.WriteString("   - Key violations found and their severity\n")
	b.WriteString("   - Any tools that were called and their key findings\n")
	b.WriteString("2. Identify patterns across multiple reviews\n")
	b.WriteString("3. Note any false positive cases or verification overrides\n\n")

	// Summary instruction — 6 dimensions.
	b.WriteString("Then, wrap your final summary in <summary> tags. Your summary MUST include:\n\n")

	b.WriteString("1. Reviewed Ads and Decisions: List every ad reviewed with its ID, region, ")
	b.WriteString("category, decision, and confidence score. This is critical for continuity.\n\n")

	b.WriteString("2. Violation Patterns and Trends: Which policies were most frequently ")
	b.WriteString("triggered? Are there recurring violation types (e.g., unverified medical ")
	b.WriteString("claims, prohibited categories)?\n\n")

	b.WriteString("3. False Positive Cases: List any cases where REJECTED was downgraded to ")
	b.WriteString("MANUAL_REVIEW (by confidence threshold, AllowAutoReject, or verification ")
	b.WriteString("override). Include the reason for each downgrade.\n\n")

	b.WriteString("4. Regional Compliance Notes: Summarize key compliance findings per region ")
	b.WriteString("(e.g., MENA_SA alcohol prohibition, EU strict data requirements). ")
	b.WriteString("Include any region-specific requirements that were flagged.\n\n")

	b.WriteString("5. Advertiser Reputation: Note any advertiser reputation observations ")
	b.WriteString("(trusted, flagged, probation) and changes across reviews.\n\n")

	b.WriteString("6. Pending Tasks: List any remaining ads that still need review, ")
	b.WriteString("or note that all ads have been processed.\n\n")

	// Trailer — reinforce no-tools constraint.
	b.WriteString("REMINDER: Do NOT call any tools. Respond with plain text only — ")
	b.WriteString("an <analysis> block followed by a <summary> block.")

	return b.String()
}

// summaryRe matches <summary>...</summary> content.
var summaryRe = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)

// analysisRe matches <analysis>...</analysis> content.
var analysisRe = regexp.MustCompile(`(?s)<analysis>[\s\S]*?</analysis>`)

// FormatCompactSummary extracts the <summary> section from LLM output,
// discarding the <analysis> scratch pad.
func FormatCompactSummary(raw string) string {
	// Try to extract <summary> content.
	if match := summaryRe.FindStringSubmatch(raw); len(match) > 1 {
		return strings.TrimSpace(match[1])
	}

	// Fallback: strip <analysis> block and return rest.
	cleaned := analysisRe.ReplaceAllString(raw, "")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned != "" {
		return cleaned
	}

	// Last resort: return raw (better than empty).
	return strings.TrimSpace(raw)
}

// BuildCompactSummaryMessage constructs the user message placed after compression.
// Contains the summary plus instructions to continue without asking questions.
func BuildCompactSummaryMessage(summary string) string {
	var b strings.Builder
	b.WriteString("This session is being continued from a previous conversation that was ")
	b.WriteString("compressed to save context space. The summary below covers the earlier ")
	b.WriteString("portion of the conversation.\n\n")
	b.WriteString("Summary:\n")
	b.WriteString(summary)
	b.WriteString("\n\nIf you need specific details from before compression, they may not ")
	b.WriteString("be available. Work with the information in the summary above.\n")
	b.WriteString("Continue reviewing without asking further questions. ")
	b.WriteString("Resume directly — do not acknowledge the summary or repeat it.")
	return b.String()
}
