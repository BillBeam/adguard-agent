package agent

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/memory"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// MemoryExtractionHook implements StopHook.
// Extracts review patterns from completed reviews and writes to AgentMemory.
// Rule-based extraction (no LLM call) — runs synchronously in BeforeStop.
type MemoryExtractionHook struct {
	mem    *memory.AgentMemory
	logger *slog.Logger
}

// NewMemoryExtractionHook creates a memory extraction hook.
func NewMemoryExtractionHook(mem *memory.AgentMemory, logger *slog.Logger) *MemoryExtractionHook {
	return &MemoryExtractionHook{mem: mem, logger: logger}
}

// BeforeStop extracts memory-worthy patterns from the completed review.
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

	ad := state.AdContent
	violations := state.PartialResult.Violations

	// 1. High-confidence violations (>= 0.9): record the violation pattern.
	for _, v := range violations {
		if v.Confidence >= 0.9 {
			key := fmt.Sprintf("violation_%s_%s_%s", v.PolicyID, ad.Region, ad.Category)
			h.mem.Add(memory.MemoryEntry{
				Role:       role,
				Key:        key,
				Value:      fmt.Sprintf("Policy %s violation: %s", v.PolicyID, truncateValue(v.Description, 100)),
				Region:     ad.Region,
				Category:   ad.Category,
				Confidence: v.Confidence,
			})
		}
	}

	// 2. MANUAL_REVIEW edge cases: record for future reference.
	if hasManualReviewTrace(state.PartialResult.AgentTrace) && len(violations) > 0 {
		descs := make([]string, 0, len(violations))
		for _, v := range violations {
			descs = append(descs, v.PolicyID+":"+truncateValue(v.Description, 40))
		}
		key := fmt.Sprintf("edge_case_%s_%s", ad.Category, ad.Region)
		h.mem.Add(memory.MemoryEntry{
			Role:     role,
			Key:      key,
			Value:    fmt.Sprintf("Edge case requiring manual review: %s", strings.Join(descs, "; ")),
			Region:   ad.Region,
			Category: ad.Category,
			Confidence: 0.5,
		})
	}

	// 3. Multi-violation patterns (2+ violations): record the combination.
	if len(violations) >= 2 {
		policyIDs := make([]string, 0, len(violations))
		for _, v := range violations {
			policyIDs = append(policyIDs, v.PolicyID)
		}
		key := fmt.Sprintf("multi_violation_%s_%s", ad.Category, ad.Region)
		h.mem.Add(memory.MemoryEntry{
			Role:       role,
			Key:        key,
			Value:      fmt.Sprintf("Multi-violation pattern: %s", strings.Join(policyIDs, "+")),
			Region:     ad.Region,
			Category:   ad.Category,
			Confidence: avgConfidence(violations),
		})
	}

	return nil
}

func hasManualReviewTrace(trace []string) bool {
	for _, t := range trace {
		if strings.Contains(t, "MANUAL_REVIEW") {
			return true
		}
	}
	return false
}

func avgConfidence(violations []types.PolicyViolation) float64 {
	if len(violations) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range violations {
		sum += v.Confidence
	}
	return sum / float64(len(violations))
}

func truncateValue(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
