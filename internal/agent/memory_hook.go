package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/memory"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// MemoryExtractionHook implements StopHook.
// After each review completes, calls LLM to analyze the review context and
// extract patterns worth remembering — advertiser behavior, Algospeak variants,
// policy precedents, regional edge cases. The LLM decides what is memory-worthy;
// this mirrors the forked-agent extraction pattern where the extraction agent
// shares the system prompt prefix (enabling prompt cache hits) and autonomously
// determines what to save.
type MemoryExtractionHook struct {
	mem    *memory.AgentMemory
	client llm.LLMClient
	logger *slog.Logger
}

// NewMemoryExtractionHook creates an LLM-driven memory extraction hook.
// The client is used for a single extraction LLM call after each review.
func NewMemoryExtractionHook(mem *memory.AgentMemory, client llm.LLMClient, logger *slog.Logger) *MemoryExtractionHook {
	return &MemoryExtractionHook{mem: mem, client: client, logger: logger}
}

// BeforeStop triggers LLM-driven memory extraction from the completed review.
// Graceful degradation: LLM errors are logged and skipped, never blocking the review.
func (h *MemoryExtractionHook) BeforeStop(state *State, reason ExitReason) error {
	if h.mem == nil || reason != ExitCompleted {
		return nil
	}
	if state.PartialResult == nil || state.AdContent == nil {
		return nil
	}

	role := state.AgentRole
	if role == "" {
		role = "single"
	}

	// When no LLM client is available (mock mode), fall back to rule-based extraction
	// so that mock demo still shows memory entries in Feature Showcase.
	if h.client == nil {
		h.extractRuleBased(state, role)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Build review context summary from state.
	reviewContext := buildReviewContext(state)
	if reviewContext == "" {
		return nil
	}

	// 2. Load existing relevant memories to avoid duplicates.
	ad := state.AdContent
	existingEntries := h.mem.LoadRelevant(role, ad.Region, ad.Category)
	existingSection := "None"
	if len(existingEntries) > 0 {
		existingSection = h.mem.FormatForPrompt(existingEntries)
	}

	// 3. Build extraction prompt.
	extractionPrompt := buildExtractionPrompt(reviewContext, existingSection)

	// 4. Call LLM — system message reuses the static prompt prefix for cache hits.
	maxTokens := 1024
	resp, err := h.client.ChatCompletion(ctx, types.ChatCompletionRequest{
		Messages: []types.Message{
			{Role: types.RoleSystem, Content: types.NewTextContent(
				buildStaticPromptPrefix() + "\n\n" + memoryExtractionSystemSuffix)},
			{Role: types.RoleUser, Content: types.NewTextContent(extractionPrompt)},
		},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		h.logger.Debug("memory extraction LLM call failed, skipping",
			slog.String("error", err.Error()))
		return nil // graceful degradation
	}

	if resp == nil || len(resp.Choices) == 0 {
		return nil
	}

	// 5. Parse LLM output and write to AgentMemory.
	raw := resp.Choices[0].Message.Content.String()
	entries := parseExtractionOutput(raw)

	for _, e := range entries {
		e.Role = role
		e.Region = ad.Region
		e.Category = ad.Category
		h.mem.Add(e)
	}

	if len(entries) > 0 {
		h.logger.Info("memory extraction completed",
			slog.String("role", role),
			slog.String("ad_id", ad.ID),
			slog.Int("entries_extracted", len(entries)),
		)
	}

	return nil
}

// memoryExtractionSystemSuffix is appended to the shared static prefix.
// This establishes the extraction agent's role without duplicating the full
// review system context (which is already in the static prefix and cached).
const memoryExtractionSystemSuffix = `You are also acting as a memory extraction agent. After a review completes, you analyze the review context and extract patterns worth remembering for future reviews. Output ONLY a JSON array.`

// buildExtractionPrompt constructs the user message for the extraction LLM call.
func buildExtractionPrompt(reviewContext, existingMemories string) string {
	var b strings.Builder

	b.WriteString("A review has just completed. Analyze the review context below and extract\n")
	b.WriteString("patterns worth remembering for future reviews.\n\n")

	b.WriteString("=== MEMORY TYPES ===\n\n")

	b.WriteString("1. advertiser_pattern: Advertiser-specific behavior patterns.\n")
	b.WriteString("   Save when: an advertiser shows a recognizable violation pattern or evasion tactic.\n")
	b.WriteString("   Example: \"Advertiser adv_001 repeatedly submits healthcare ads with unverified\n")
	b.WriteString("   efficacy claims disguised as user testimonials.\"\n\n")

	b.WriteString("2. algospeak_variant: Algospeak evasion patterns detected.\n")
	b.WriteString("   Save when: a new Algospeak encoding or variant is detected in content analysis.\n")
	b.WriteString("   Example: \"In weight_loss category, 'wei減' decodes to 'weight loss' using\n")
	b.WriteString("   mixed-script CJK substitution.\"\n\n")

	b.WriteString("3. policy_precedent: Policy interpretation precedents.\n")
	b.WriteString("   Save when: a borderline policy interpretation decision was made (not obvious from policy text).\n")
	b.WriteString("   Example: \"POL_001 in EU/healthcare: 'clinically tested' without specifying\n")
	b.WriteString("   the study is insufficient — requires specific trial reference.\"\n\n")

	b.WriteString("4. region_edge_case: Region-specific compliance edge cases.\n")
	b.WriteString("   Save when: a region-specific rule produced a non-obvious outcome.\n")
	b.WriteString("   Example: \"MENA_SA/alcohol: even 0% ABV beverages are prohibited — policy\n")
	b.WriteString("   covers all alcohol references including non-alcoholic variants.\"\n\n")

	b.WriteString("=== WHAT NOT TO SAVE ===\n")
	b.WriteString("- Routine PASSED decisions with no notable patterns\n")
	b.WriteString("- Violations that are obvious from the policy text alone\n")
	b.WriteString("- Information already captured in existing memories below\n")
	b.WriteString("- Generic facts about categories or regions (e.g., \"alcohol is restricted\")\n\n")

	b.WriteString("=== EXISTING RELEVANT MEMORIES ===\n")
	b.WriteString(existingMemories)
	b.WriteString("\n\n")

	b.WriteString("=== REVIEW CONTEXT ===\n")
	b.WriteString(reviewContext)
	b.WriteString("\n\n")

	b.WriteString("=== OUTPUT ===\n")
	b.WriteString("Output ONLY a JSON array of memories to save. Empty array [] if nothing worth remembering.\n")
	b.WriteString(`[{"type":"advertiser_pattern|algospeak_variant|policy_precedent|region_edge_case",`)
	b.WriteString(`"key":"unique_dedup_key","value":"concise pattern description"}]`)
	b.WriteString("\n")

	return b.String()
}

// buildReviewContext extracts a summary of the review from state for the extraction prompt.
func buildReviewContext(state *State) string {
	ad := state.AdContent
	if ad == nil {
		return ""
	}

	var b strings.Builder

	// Ad summary.
	fmt.Fprintf(&b, "Ad ID: %s | Advertiser: %s | Region: %s | Category: %s\n",
		ad.ID, ad.AdvertiserID, ad.Region, ad.Category)
	fmt.Fprintf(&b, "Headline: %s\n", ad.Content.Headline)
	fmt.Fprintf(&b, "Body: %s\n\n", ad.Content.Body)

	// Tool results summary — extract key findings from tool_result messages.
	b.WriteString("Tool Results:\n")
	toolResultCount := 0
	for _, msg := range state.Messages {
		if msg.Role != types.RoleTool {
			continue
		}
		content := msg.Content.String()
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		b.WriteString(content)
		b.WriteString("\n")
		toolResultCount++
	}
	if toolResultCount == 0 {
		b.WriteString("(no tool results)\n")
	}

	// Final decision and violations.
	violations := state.PartialResult.Violations
	if len(violations) > 0 {
		b.WriteString("\nViolations:\n")
		for _, v := range violations {
			fmt.Fprintf(&b, "- [%s] severity=%s: %s (confidence=%.2f)\n",
				v.PolicyID, v.Severity, v.Description, v.Confidence)
		}
	}

	// Agent trace highlights.
	for _, t := range state.PartialResult.AgentTrace {
		if strings.Contains(t, "MANUAL_REVIEW") || strings.Contains(t, "downgraded") ||
			strings.Contains(t, "verification") {
			fmt.Fprintf(&b, "Trace: %s\n", t)
		}
	}

	return b.String()
}

// extractedMemory is the JSON structure expected from the LLM extraction output.
type extractedMemory struct {
	Type  string `json:"type"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

// parseExtractionOutput parses the LLM's JSON array output into MemoryEntry slice.
// Handles: raw JSON array, markdown-fenced JSON, or embedded [...] in text.
func parseExtractionOutput(raw string) []memory.MemoryEntry {
	raw = strings.TrimSpace(raw)

	var extracted []extractedMemory

	// Try direct JSON array parse.
	if err := json.Unmarshal([]byte(raw), &extracted); err == nil {
		return convertExtracted(extracted)
	}

	// Try extracting from markdown fences.
	if idx := strings.Index(raw, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(raw[start:], "```"); end >= 0 {
			candidate := strings.TrimSpace(raw[start : start+end])
			if err := json.Unmarshal([]byte(candidate), &extracted); err == nil {
				return convertExtracted(extracted)
			}
		}
	}

	// Try first [...] block.
	if start := strings.Index(raw, "["); start >= 0 {
		if end := strings.LastIndex(raw, "]"); end > start {
			candidate := raw[start : end+1]
			if err := json.Unmarshal([]byte(candidate), &extracted); err == nil {
				return convertExtracted(extracted)
			}
		}
	}

	return nil
}

// extractRuleBased is the fallback for mock mode (no LLM client).
// Extracts basic patterns from structured ReviewResult so mock demo shows memory entries.
func (h *MemoryExtractionHook) extractRuleBased(state *State, role string) {
	ad := state.AdContent
	violations := state.PartialResult.Violations

	// High-confidence violations: record per advertiser+policy.
	for _, v := range violations {
		if v.Confidence >= 0.8 {
			key := fmt.Sprintf("violation_%s_%s_%s_%s", ad.AdvertiserID, v.PolicyID, ad.Region, ad.Category)
			h.mem.Add(memory.MemoryEntry{
				Role:       role,
				Key:        key,
				Value:      fmt.Sprintf("[advertiser_pattern] %s: %s violation %s", ad.AdvertiserID, ad.Category, v.PolicyID),
				Region:     ad.Region,
				Category:   ad.Category,
				Confidence: v.Confidence,
			})
		}
	}

	// Multi-violation pattern.
	if len(violations) >= 2 {
		ids := make([]string, 0, len(violations))
		for _, v := range violations {
			ids = append(ids, v.PolicyID)
		}
		key := fmt.Sprintf("multi_%s_%s_%s", ad.AdvertiserID, ad.Category, ad.Region)
		h.mem.Add(memory.MemoryEntry{
			Role:       role,
			Key:        key,
			Value:      fmt.Sprintf("[advertiser_pattern] %s multi-violation: %s", ad.AdvertiserID, strings.Join(ids, "+")),
			Region:     ad.Region,
			Category:   ad.Category,
			Confidence: 0.7,
		})
	}
}

func convertExtracted(items []extractedMemory) []memory.MemoryEntry {
	entries := make([]memory.MemoryEntry, 0, len(items))
	for _, item := range items {
		if item.Key == "" || item.Value == "" {
			continue
		}
		// Validate type — only accept known memory types.
		switch item.Type {
		case "advertiser_pattern", "algospeak_variant", "policy_precedent", "region_edge_case":
		default:
			continue
		}
		entries = append(entries, memory.MemoryEntry{
			Key:        item.Key,
			Value:      fmt.Sprintf("[%s] %s", item.Type, item.Value),
			Confidence: 0.8, // LLM-extracted memories default to 0.8 confidence
		})
	}
	return entries
}
