package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProcessResult_UnderLimit(t *testing.T) {
	b := NewResultBudget(t.TempDir(), nil)
	input := strings.Repeat("x", DefaultPerToolLimit-1)
	got := b.ProcessResult("test_tool", "call_001", input)
	if got != input {
		t.Error("expected result unchanged when under limit")
	}
}

func TestProcessResult_ExactLimit(t *testing.T) {
	b := NewResultBudget(t.TempDir(), nil)
	input := strings.Repeat("x", DefaultPerToolLimit)
	got := b.ProcessResult("test_tool", "call_002", input)
	if got != input {
		t.Error("expected result unchanged at exact limit")
	}
}

func TestProcessResult_OverLimit(t *testing.T) {
	dir := t.TempDir()
	b := NewResultBudget(dir, nil)
	input := strings.Repeat("a", 50000) // 50KB > 32KB
	got := b.ProcessResult("test_tool", "call_003", input)

	// Should contain persisted output tags.
	if !strings.Contains(got, "<persisted-output>") {
		t.Error("expected <persisted-output> tag")
	}
	if !strings.Contains(got, "</persisted-output>") {
		t.Error("expected </persisted-output> closing tag")
	}
	if !strings.Contains(got, "Output too large") {
		t.Error("expected size message")
	}

	// Preview should be roughly PreviewSize.
	if len(got) > 5000 {
		t.Errorf("preview too large: %d bytes (expected ~%d)", len(got), DefaultPreviewSize)
	}

	// File should exist on disk.
	path := filepath.Join(dir, "tool-results", "call_003.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("persisted file not found: %v", err)
	}
	if len(data) != 50000 {
		t.Errorf("persisted file size = %d, want 50000", len(data))
	}
}

func TestProcessResult_Idempotent(t *testing.T) {
	dir := t.TempDir()
	b := NewResultBudget(dir, nil)
	input := strings.Repeat("b", 50000)

	// Call twice with same toolCallID — should not fail on second call.
	got1 := b.ProcessResult("test_tool", "call_004", input)
	got2 := b.ProcessResult("test_tool", "call_004", input)
	if got1 != got2 {
		t.Error("expected idempotent result for same toolCallID")
	}
}

func TestProcessRound_UnderLimit(t *testing.T) {
	b := NewResultBudget(t.TempDir(), nil)
	results := []RoundResult{
		{ToolCallID: "c1", ToolName: "t1", Content: strings.Repeat("a", 50000)},
		{ToolCallID: "c2", ToolName: "t2", Content: strings.Repeat("b", 50000)},
		{ToolCallID: "c3", ToolName: "t3", Content: strings.Repeat("c", 50000)},
	}
	// Total: 150KB < 200KB (DefaultPerRoundLimit)
	got := b.ProcessRound(results)
	for i, r := range got {
		if r.Content != results[i].Content {
			t.Errorf("result[%d] changed when total under limit", i)
		}
	}
}

func TestProcessRound_OverLimit(t *testing.T) {
	dir := t.TempDir()
	b := NewResultBudget(dir, nil)
	results := []RoundResult{
		{ToolCallID: "c1", ToolName: "t1", Content: strings.Repeat("a", 100000)}, // 100KB — largest
		{ToolCallID: "c2", ToolName: "t2", Content: strings.Repeat("b", 80000)},  // 80KB
		{ToolCallID: "c3", ToolName: "t3", Content: strings.Repeat("c", 50000)},  // 50KB
	}
	// Total: 230KB > 200KB. Largest (100KB) should be evicted.
	got := b.ProcessRound(results)

	// Largest result should be replaced with preview.
	if !strings.Contains(got[0].Content, "<persisted-output>") {
		t.Error("expected largest result to be persisted")
	}
	// Smaller results should be unchanged.
	if got[1].Content != results[1].Content {
		t.Error("80KB result should not be evicted")
	}
	if got[2].Content != results[2].Content {
		t.Error("50KB result should not be evicted")
	}
}

func TestGeneratePreview_NewlineTruncation(t *testing.T) {
	content := strings.Repeat("line content here\n", 200) // ~3.6KB
	preview := generatePreview(content, 2048)

	if len(preview) > 2048 {
		t.Errorf("preview too large: %d > 2048", len(preview))
	}
	// Should not cut mid-line — the last character should be either
	// a newline or the character just before a newline boundary.
	// LastIndex returns the position OF the newline, so truncation
	// at that position gives content[:pos] which ends before \n.
	// Verify no partial line at the end.
	lines := strings.Split(preview, "\n")
	lastLine := lines[len(lines)-1]
	if len(lastLine) > len("line content here") {
		t.Errorf("preview appears to cut mid-line: last line = %q", lastLine)
	}
}

func TestGeneratePreview_MidLineFallback(t *testing.T) {
	// Content with no newlines — forced to cut mid-content.
	content := strings.Repeat("x", 5000)
	preview := generatePreview(content, 2048)

	if len(preview) != 2048 {
		t.Errorf("preview length = %d, want 2048", len(preview))
	}
}

func TestGeneratePreview_ShortContent(t *testing.T) {
	content := "short content"
	preview := generatePreview(content, 2048)
	if preview != content {
		t.Error("short content should be returned unchanged")
	}
}

func TestGeneratePreview_HTMLExtraction(t *testing.T) {
	html := `<!DOCTYPE html>
<html>
<head>
<title>Amazing Weight Loss Product</title>
<meta name="description" content="Lose 30 pounds in 30 days guaranteed">
</head>
<body>
<p>Privacy policy section...</p>
` + strings.Repeat("<p>filler content</p>\n", 500)

	preview := generatePreview(html, 2048)
	if !strings.Contains(preview, "Key Signals") {
		t.Error("expected key signals section for HTML")
	}
	if !strings.Contains(preview, "title: Amazing Weight Loss Product") {
		t.Error("expected title extraction")
	}
	if !strings.Contains(preview, "meta_description: Lose 30 pounds") {
		t.Error("expected meta description extraction")
	}
	if !strings.Contains(preview, "privacy_policy: detected") {
		t.Error("expected privacy policy detection")
	}
}

func TestLooksLikeHTML(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"<!DOCTYPE html><html>", true},
		{"<html><head>", true},
		{"<HTML><BODY>", true},
		{`{"json": "data"}`, false},
		{"plain text", false},
	}
	for _, tt := range tests {
		got := looksLikeHTML(tt.input)
		if got != tt.want {
			t.Errorf("looksLikeHTML(%q) = %v, want %v", tt.input[:20], got, tt.want)
		}
	}
}
