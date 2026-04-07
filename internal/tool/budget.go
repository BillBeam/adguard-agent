// Tool Result Budget: two-layer size control for tool execution results.
//
// Layer 1 (per-tool): Individual results exceeding the per-tool limit are
// persisted to disk with an inline preview. This prevents large landing page
// HTML from consuming the entire context window.
//
// Layer 2 (per-round): When multiple tools execute in one round and their
// combined output exceeds the per-round limit, the largest results are
// iteratively persisted until the total fits within budget.
//
// The preview uses smart truncation: preferring newline boundaries and
// extracting key HTML signals (title, meta description) when applicable.
package tool

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// DefaultPerToolLimit is the maximum size (bytes) for a single tool result
	// before it is persisted to disk. 32KB is sufficient for most structured
	// JSON outputs; landing pages (50-200KB HTML) are the primary trigger.
	DefaultPerToolLimit = 32768

	// DefaultPerRoundLimit is the maximum aggregate size (bytes) for all tool
	// results in one round. Prevents context explosion when multiple tools
	// return large results simultaneously (e.g., 3 landing page checks).
	DefaultPerRoundLimit = 204800 // 200KB

	// DefaultPreviewSize is the size (bytes) of the inline preview generated
	// for persisted results. 2KB provides enough context for the LLM to
	// understand the content without consuming excessive context.
	DefaultPreviewSize = 2048
)

// ResultBudget manages tool result sizes with two-layer control.
type ResultBudget struct {
	PerToolLimit  int
	PerRoundLimit int
	PreviewSize   int
	PersistDir    string
	logger        *slog.Logger
}

// NewResultBudget creates a budget with default limits.
// persistDir is the directory where oversized results are written.
func NewResultBudget(persistDir string, logger *slog.Logger) *ResultBudget {
	return &ResultBudget{
		PerToolLimit:  DefaultPerToolLimit,
		PerRoundLimit: DefaultPerRoundLimit,
		PreviewSize:   DefaultPreviewSize,
		PersistDir:    persistDir,
		logger:        logger,
	}
}

// RoundResult represents a tool result eligible for per-round budget evaluation.
type RoundResult struct {
	ToolCallID string
	ToolName   string
	Content    string
}

// ProcessResult applies Layer 1: per-tool size check.
// Results within budget are returned unchanged.
// Oversized results are persisted to disk and replaced with an inline preview.
func (b *ResultBudget) ProcessResult(toolName, toolCallID, result string) string {
	if len(result) <= b.PerToolLimit {
		return result
	}
	return b.persistAndPreview(toolName, toolCallID, result)
}

// ProcessRound applies Layer 2: per-round aggregate check.
// If the total size of all results exceeds PerRoundLimit, the largest results
// are iteratively persisted to disk until the total fits within budget.
func (b *ResultBudget) ProcessRound(results []RoundResult) []RoundResult {
	totalSize := 0
	for _, r := range results {
		totalSize += len(r.Content)
	}
	if totalSize <= b.PerRoundLimit {
		return results
	}

	// Build index sorted by size (largest first) for greedy eviction.
	type indexed struct {
		idx  int
		size int
	}
	order := make([]indexed, len(results))
	for i, r := range results {
		order[i] = indexed{idx: i, size: len(r.Content)}
	}
	sort.Slice(order, func(i, j int) bool {
		return order[i].size > order[j].size
	})

	// Evict largest results until within budget.
	out := make([]RoundResult, len(results))
	copy(out, results)
	remaining := totalSize
	for _, o := range order {
		if remaining <= b.PerRoundLimit {
			break
		}
		r := &out[o.idx]
		original := len(r.Content)
		r.Content = b.persistAndPreview(r.ToolName, r.ToolCallID, r.Content)
		remaining -= original - len(r.Content)
		b.log().Info("per-round budget: evicted large result",
			slog.String("tool", r.ToolName),
			slog.Int("original_bytes", original),
			slog.Int("preview_bytes", len(r.Content)),
		)
	}
	return out
}

func (b *ResultBudget) log() *slog.Logger {
	if b.logger != nil {
		return b.logger
	}
	return slog.Default()
}

// persistAndPreview writes the full result to disk and returns an inline preview.
func (b *ResultBudget) persistAndPreview(toolName, toolCallID, result string) string {
	dir := filepath.Join(b.PersistDir, "tool-results")
	if err := os.MkdirAll(dir, 0755); err != nil {
		b.log().Error("failed to create tool-results dir", slog.String("error", err.Error()))
		return truncateFallback(result, b.PerToolLimit)
	}

	path := filepath.Join(dir, toolCallID+".txt")
	// O_EXCL: skip if already persisted (idempotent).
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil && !os.IsExist(err) {
		b.log().Error("failed to persist tool result",
			slog.String("path", path), slog.String("error", err.Error()))
		return truncateFallback(result, b.PerToolLimit)
	}
	if err == nil {
		_, writeErr := f.WriteString(result)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			b.log().Error("failed to write tool result to disk",
				slog.String("path", path),
				slog.Any("write_error", writeErr),
				slog.Any("close_error", closeErr))
			// File may be partial — remove it so retry doesn't skip via O_EXCL.
			os.Remove(path)
			return truncateFallback(result, b.PerToolLimit)
		}
	}

	preview := generatePreview(result, b.PreviewSize)
	sizeKB := len(result) / 1024

	b.log().Info("tool result persisted",
		slog.String("tool", toolName),
		slog.Int("size_kb", sizeKB),
		slog.String("path", path),
	)

	var sb strings.Builder
	sb.WriteString("<persisted-output>\n")
	fmt.Fprintf(&sb, "Output too large (%dKB). Full output saved to: %s\n\n", sizeKB, path)
	fmt.Fprintf(&sb, "Preview (first %dB):\n", b.PreviewSize)
	sb.WriteString(preview)
	if len(result) > b.PreviewSize {
		sb.WriteString("\n...")
	}
	sb.WriteString("\n</persisted-output>")
	return sb.String()
}

// generatePreview creates a smart preview of the content.
// Prefers truncation at newline boundaries. For HTML content,
// extracts key signals (title, meta description) into the preview.
func generatePreview(content string, maxSize int) string {
	if len(content) <= maxSize {
		return content
	}

	// For HTML: prepend extracted signals before the raw preview.
	var prefix string
	if looksLikeHTML(content) {
		signals := extractHTMLSignals(content)
		if signals != "" {
			prefix = "--- Key Signals ---\n" + signals + "\n--- Raw Preview ---\n"
			maxSize -= len(prefix)
			if maxSize < 256 {
				maxSize = 256 // minimum raw preview
			}
		}
	}

	truncated := content[:maxSize]
	lastNewline := strings.LastIndex(truncated, "\n")

	// If newline found reasonably close to limit (>50%), cut there.
	if lastNewline > maxSize/2 {
		return prefix + content[:lastNewline]
	}
	return prefix + truncated
}

// truncateFallback is the last-resort truncation when disk persistence fails.
// Respects UTF-8 boundaries to avoid producing invalid strings.
func truncateFallback(result string, maxSize int) string {
	suffix := "\n...[truncated, disk persist failed]"
	cutAt := maxSize - len(suffix)
	if cutAt < 0 {
		cutAt = 0
	}
	if cutAt > len(result) {
		cutAt = len(result)
	}
	// Walk back to a valid UTF-8 boundary (avoid splitting multi-byte chars).
	for cutAt > 0 && result[cutAt-1]&0xC0 == 0x80 {
		cutAt--
	}
	return result[:cutAt] + suffix
}

// looksLikeHTML returns true if content appears to be HTML.
func looksLikeHTML(content string) bool {
	lower := strings.ToLower(content[:min(500, len(content))])
	return strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype") ||
		strings.Contains(lower, "<head") || strings.Contains(lower, "<body")
}

var (
	titleRe = regexp.MustCompile(`(?i)<title[^>]*>(.*?)</title>`)
	metaRe  = regexp.MustCompile(`(?i)<meta\s+name=["']description["']\s+content=["']([^"']*)["']`)
)

// extractHTMLSignals pulls title and meta description from HTML content.
func extractHTMLSignals(content string) string {
	var signals []string
	if m := titleRe.FindStringSubmatch(content); len(m) > 1 {
		signals = append(signals, "title: "+strings.TrimSpace(m[1]))
	}
	if m := metaRe.FindStringSubmatch(content); len(m) > 1 {
		signals = append(signals, "meta_description: "+strings.TrimSpace(m[1]))
	}
	if strings.Contains(strings.ToLower(content), "privacy") {
		signals = append(signals, "privacy_policy: detected")
	}
	return strings.Join(signals, "\n")
}
