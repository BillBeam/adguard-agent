// Package memory implements per-role Agent Memory for cross-review learning.
//
// Each agent role (content, policy, region) accumulates patterns from completed
// reviews. Relevant memories are injected into the system prompt dynamic section
// on subsequent reviews, enabling the agent to recognize recurring violations,
// advertiser behavior patterns, and regional edge cases.
//
// Storage: one JSONL file per role, startup recovery, LRU eviction at max capacity.
package memory

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BillBeam/adguard-agent/internal/store"
)

// MemoryEntry represents one learned pattern from a previous review.
type MemoryEntry struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`       // "content", "policy", "region", "single"
	Key        string    `json:"key"`        // dedup key, e.g. "violation_POL_001_US_healthcare"
	Value      string    `json:"value"`      // human-readable pattern description
	Region     string    `json:"region"`     // filter dimension
	Category   string    `json:"category"`   // filter dimension
	Confidence float64   `json:"confidence"` // extraction confidence
	UpdatedAt  time.Time `json:"updated_at"`
}

const (
	maxRelevantEntries = 20 // max entries injected into a single prompt
)

// AgentMemory manages per-role memory entries with JSONL persistence.
type AgentMemory struct {
	mu      sync.RWMutex
	entries map[string]*MemoryEntry     // key -> entry (dedup)
	order   []string                    // LRU order (oldest first)
	maxSize int                         // max entries per role
	writers map[string]*store.JSONLWriter // role -> JSONL writer
	logger  *slog.Logger
}

// NewAgentMemory creates a memory store. If dataDir is non-empty, enables
// JSONL persistence and recovers existing entries on startup.
func NewAgentMemory(dataDir string, maxSize int, logger *slog.Logger) *AgentMemory {
	m := &AgentMemory{
		entries: make(map[string]*MemoryEntry),
		order:   make([]string, 0, maxSize),
		maxSize: maxSize,
		writers: make(map[string]*store.JSONLWriter),
		logger:  logger,
	}

	if dataDir == "" {
		return m
	}

	// Recover existing memories from JSONL files per role.
	roles := []string{"content", "policy", "region", "single"}
	for _, role := range roles {
		path := filepath.Join(dataDir, fmt.Sprintf("memory_%s.jsonl", role))
		records, skipped, err := store.ReadJSONL[MemoryEntry](path)
		if err != nil {
			logger.Debug("no existing memory file", slog.String("role", role))
		}
		for i := range records {
			r := &records[i]
			m.entries[r.Key] = r
			m.order = append(m.order, r.Key)
		}
		if len(records) > 0 || skipped > 0 {
			logger.Info("restored agent memories",
				slog.String("role", role),
				slog.Int("count", len(records)),
				slog.Int("skipped", skipped),
			)
		}

		// Open writer for new entries.
		w, err := store.NewJSONLWriter(path, logger)
		if err != nil {
			logger.Warn("failed to open memory JSONL writer",
				slog.String("role", role), slog.String("error", err.Error()))
			continue
		}
		m.writers[role] = w
	}

	return m
}

// Add inserts or updates a memory entry. Deduplicates by Key.
// LRU eviction when exceeding maxSize for the entry's role.
func (m *AgentMemory) Add(entry MemoryEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = time.Now()
	}
	if entry.ID == "" {
		entry.ID = entry.Key
	}

	if existing, ok := m.entries[entry.Key]; ok {
		// Update existing entry.
		existing.Value = entry.Value
		existing.Confidence = entry.Confidence
		existing.UpdatedAt = entry.UpdatedAt
		// Move to end of LRU order.
		m.removeLRU(entry.Key)
		m.order = append(m.order, entry.Key)
	} else {
		// New entry.
		m.entries[entry.Key] = &entry
		m.order = append(m.order, entry.Key)
	}

	// LRU eviction: count entries for this role, evict oldest if over limit.
	roleCount := 0
	for _, e := range m.entries {
		if e.Role == entry.Role {
			roleCount++
		}
	}
	for roleCount > m.maxSize {
		// Find oldest entry for this role in LRU order.
		for i, key := range m.order {
			if e, ok := m.entries[key]; ok && e.Role == entry.Role {
				delete(m.entries, key)
				m.order = append(m.order[:i], m.order[i+1:]...)
				roleCount--
				break
			}
		}
	}

	// Persist.
	if w, ok := m.writers[entry.Role]; ok {
		w.Append(&entry)
	}

	m.logger.Debug("agent memory updated",
		slog.String("role", entry.Role),
		slog.String("key", entry.Key),
	)
}

// removeLRU removes a key from the LRU order slice.
func (m *AgentMemory) removeLRU(key string) {
	for i, k := range m.order {
		if k == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			return
		}
	}
}

// LoadRelevant returns memory entries matching the given role and context.
// Filters by role, then by region/category relevance. Returns up to maxRelevantEntries.
func (m *AgentMemory) LoadRelevant(role, region, category string) []MemoryEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var relevant []MemoryEntry
	for _, e := range m.entries {
		if e.Role != role {
			continue
		}
		// Region match: exact, Global, or empty (matches all).
		regionMatch := e.Region == "" || e.Region == "Global" || e.Region == region ||
			strings.HasPrefix(region, e.Region+"_")
		// Category match: exact or empty (matches all).
		categoryMatch := e.Category == "" || e.Category == category
		if regionMatch && categoryMatch {
			relevant = append(relevant, *e)
		}
	}

	// Sort by confidence descending.
	sort.Slice(relevant, func(i, j int) bool {
		return relevant[i].Confidence > relevant[j].Confidence
	})

	if len(relevant) > maxRelevantEntries {
		relevant = relevant[:maxRelevantEntries]
	}
	return relevant
}

// FormatForPrompt builds the memory section for injection into a system prompt.
// Returns empty string if no entries.
func (m *AgentMemory) FormatForPrompt(entries []MemoryEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("=== AGENT MEMORY ===\n")
	b.WriteString("Patterns learned from previous reviews (use as reference, not as sole basis for decisions):\n")
	for _, e := range entries {
		region := e.Region
		if region == "" {
			region = "Global"
		}
		category := e.Category
		if category == "" {
			category = "all"
		}
		fmt.Fprintf(&b, "- [%s/%s] %s (conf=%.2f)\n", category, region, e.Value, e.Confidence)
	}
	return b.String()
}

// Stats returns per-role entry counts for Feature Showcase display.
func (m *AgentMemory) Stats() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	counts := make(map[string]int)
	for _, e := range m.entries {
		counts[e.Role]++
	}
	return counts
}

// TotalCount returns the total number of memory entries.
func (m *AgentMemory) TotalCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

// Flush writes all pending data to disk.
func (m *AgentMemory) Flush() {
	for _, w := range m.writers {
		w.Flush()
	}
}
