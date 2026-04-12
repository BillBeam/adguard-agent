package agent

import (
	"testing"

	"github.com/BillBeam/adguard-agent/internal/memory"
)

func TestParseExtractionOutput_DirectJSON(t *testing.T) {
	raw := `[{"type":"advertiser_pattern","key":"adv_001_healthcare","value":"repeated violations"}]`
	entries := parseExtractionOutput(raw)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Key != "adv_001_healthcare" {
		t.Errorf("Key = %q, want adv_001_healthcare", entries[0].Key)
	}
}

func TestParseExtractionOutput_MarkdownFenced(t *testing.T) {
	raw := "Here are the memories:\n```json\n" +
		`[{"type":"algospeak_variant","key":"wei_jian","value":"mixed-script weight loss"}]` +
		"\n```\nDone."
	entries := parseExtractionOutput(raw)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry from markdown fence, got %d", len(entries))
	}
	if entries[0].Key != "wei_jian" {
		t.Errorf("Key = %q, want wei_jian", entries[0].Key)
	}
}

func TestParseExtractionOutput_EmbeddedArray(t *testing.T) {
	raw := `Based on the review, I found: [{"type":"region_edge_case","key":"mena_alcohol","value":"0% ABV prohibited"}] end.`
	entries := parseExtractionOutput(raw)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry from embedded array, got %d", len(entries))
	}
}

func TestParseExtractionOutput_EmptyArray(t *testing.T) {
	entries := parseExtractionOutput("[]")
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty array, got %d", len(entries))
	}
}

func TestParseExtractionOutput_InvalidJSON(t *testing.T) {
	entries := parseExtractionOutput("this is not json at all")
	if entries != nil {
		t.Errorf("expected nil for invalid JSON, got %v", entries)
	}
}

func TestParseExtractionOutput_MultipleEntries(t *testing.T) {
	raw := `[
		{"type":"advertiser_pattern","key":"k1","value":"pattern A"},
		{"type":"policy_precedent","key":"k2","value":"precedent B"},
		{"type":"algospeak_variant","key":"k3","value":"variant C"}
	]`
	entries := parseExtractionOutput(raw)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
}

func TestConvertExtracted_ValidTypes(t *testing.T) {
	items := []extractedMemory{
		{Type: "advertiser_pattern", Key: "k1", Value: "v1"},
		{Type: "algospeak_variant", Key: "k2", Value: "v2"},
		{Type: "policy_precedent", Key: "k3", Value: "v3"},
		{Type: "region_edge_case", Key: "k4", Value: "v4"},
	}
	entries := convertExtracted(items)
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries for 4 valid types, got %d", len(entries))
	}
}

func TestConvertExtracted_InvalidType(t *testing.T) {
	items := []extractedMemory{
		{Type: "unknown_type", Key: "k1", Value: "v1"},
		{Type: "advertiser_pattern", Key: "k2", Value: "v2"},
	}
	entries := convertExtracted(items)
	if len(entries) != 1 {
		t.Errorf("expected 1 entry (invalid type filtered), got %d", len(entries))
	}
	if len(entries) > 0 && entries[0].Key != "k2" {
		t.Errorf("Key = %q, want k2", entries[0].Key)
	}
}

func TestConvertExtracted_EmptyKeyValue(t *testing.T) {
	items := []extractedMemory{
		{Type: "advertiser_pattern", Key: "", Value: "v1"},
		{Type: "advertiser_pattern", Key: "k2", Value: ""},
		{Type: "advertiser_pattern", Key: "k3", Value: "v3"},
	}
	entries := convertExtracted(items)
	if len(entries) != 1 {
		t.Errorf("expected 1 entry (empty key/value filtered), got %d", len(entries))
	}
}

func TestConvertExtracted_DefaultConfidence(t *testing.T) {
	items := []extractedMemory{
		{Type: "policy_precedent", Key: "k1", Value: "v1"},
	}
	entries := convertExtracted(items)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Confidence != 0.8 {
		t.Errorf("Confidence = %f, want 0.8", entries[0].Confidence)
	}
}

// Verify the MemoryEntry type is correctly used.
var _ memory.MemoryEntry = memory.MemoryEntry{}
