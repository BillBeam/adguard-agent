package memory

import (
	"log/slog"
	"os"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestAgentMemory_AddAndLoad(t *testing.T) {
	m := NewAgentMemory("", 200, testLogger())
	m.Add(MemoryEntry{Role: "content", Key: "k1", Value: "pattern A", Region: "US", Category: "healthcare", Confidence: 0.9})
	m.Add(MemoryEntry{Role: "policy", Key: "k2", Value: "pattern B", Region: "US", Category: "healthcare", Confidence: 0.8})
	m.Add(MemoryEntry{Role: "content", Key: "k3", Value: "pattern C", Region: "EU", Category: "alcohol", Confidence: 0.7})

	// Load content/US/healthcare — should get k1 only.
	entries := m.LoadRelevant("content", "US", "healthcare")
	if len(entries) != 1 {
		t.Errorf("expected 1 content entry for US/healthcare, got %d", len(entries))
	}
	if len(entries) > 0 && entries[0].Key != "k1" {
		t.Errorf("expected k1, got %s", entries[0].Key)
	}

	// Load policy — should get k2.
	entries = m.LoadRelevant("policy", "US", "healthcare")
	if len(entries) != 1 {
		t.Errorf("expected 1 policy entry, got %d", len(entries))
	}
}

func TestAgentMemory_Dedup(t *testing.T) {
	m := NewAgentMemory("", 200, testLogger())
	m.Add(MemoryEntry{Role: "content", Key: "k1", Value: "old value", Confidence: 0.5})
	m.Add(MemoryEntry{Role: "content", Key: "k1", Value: "new value", Confidence: 0.9})

	if m.TotalCount() != 1 {
		t.Errorf("expected 1 entry after dedup, got %d", m.TotalCount())
	}

	entries := m.LoadRelevant("content", "", "")
	if len(entries) != 1 || entries[0].Value != "new value" {
		t.Errorf("expected updated value 'new value', got %q", entries[0].Value)
	}
	if entries[0].Confidence != 0.9 {
		t.Errorf("expected updated confidence 0.9, got %.1f", entries[0].Confidence)
	}
}

func TestAgentMemory_LRUEviction(t *testing.T) {
	m := NewAgentMemory("", 3, testLogger())
	m.Add(MemoryEntry{Role: "content", Key: "k1", Value: "v1"})
	m.Add(MemoryEntry{Role: "content", Key: "k2", Value: "v2"})
	m.Add(MemoryEntry{Role: "content", Key: "k3", Value: "v3"})
	m.Add(MemoryEntry{Role: "content", Key: "k4", Value: "v4"})
	m.Add(MemoryEntry{Role: "content", Key: "k5", Value: "v5"})

	// Only 3 most recent should remain for "content" role.
	entries := m.LoadRelevant("content", "", "")
	if len(entries) != 3 {
		t.Errorf("expected 3 entries after LRU eviction, got %d", len(entries))
	}

	// k1 and k2 should be evicted.
	keys := map[string]bool{}
	for _, e := range entries {
		keys[e.Key] = true
	}
	if keys["k1"] || keys["k2"] {
		t.Error("k1 and k2 should have been evicted")
	}
	if !keys["k3"] || !keys["k4"] || !keys["k5"] {
		t.Error("k3, k4, k5 should remain")
	}
}

func TestAgentMemory_FormatForPrompt(t *testing.T) {
	m := NewAgentMemory("", 200, testLogger())

	// Empty entries → empty string.
	if s := m.FormatForPrompt(nil); s != "" {
		t.Errorf("expected empty string for nil entries, got %q", s)
	}

	entries := []MemoryEntry{
		{Key: "k1", Value: "pattern A", Region: "US", Category: "healthcare", Confidence: 0.95},
		{Key: "k2", Value: "pattern B", Region: "", Category: "", Confidence: 0.80},
	}
	result := m.FormatForPrompt(entries)
	if result == "" {
		t.Fatal("expected non-empty prompt section")
	}
	if !contains(result, "=== AGENT MEMORY ===") {
		t.Error("missing header")
	}
	if !contains(result, "healthcare/US") {
		t.Error("missing region/category tag")
	}
	if !contains(result, "pattern A") {
		t.Error("missing value")
	}
}

func TestAgentMemory_Stats(t *testing.T) {
	m := NewAgentMemory("", 200, testLogger())
	m.Add(MemoryEntry{Role: "content", Key: "k1", Value: "v1"})
	m.Add(MemoryEntry{Role: "content", Key: "k2", Value: "v2"})
	m.Add(MemoryEntry{Role: "policy", Key: "k3", Value: "v3"})

	stats := m.Stats()
	if stats["content"] != 2 {
		t.Errorf("content count = %d, want 2", stats["content"])
	}
	if stats["policy"] != 1 {
		t.Errorf("policy count = %d, want 1", stats["policy"])
	}
	if m.TotalCount() != 3 {
		t.Errorf("total = %d, want 3", m.TotalCount())
	}
}

func TestAgentMemory_RegionPrefixMatch(t *testing.T) {
	m := NewAgentMemory("", 200, testLogger())
	m.Add(MemoryEntry{Role: "region", Key: "k1", Value: "MENA pattern", Region: "MENA", Category: "alcohol", Confidence: 0.9})

	// MENA_SA should match MENA prefix.
	entries := m.LoadRelevant("region", "MENA_SA", "alcohol")
	if len(entries) != 1 {
		t.Errorf("expected 1 entry for MENA_SA matching MENA prefix, got %d", len(entries))
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
