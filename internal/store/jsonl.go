// JSONL persistence: append-only log files for crash-safe data storage.
//
// Each store (ReviewStore, AppealStore, TrainingPool) maintains its own JSONL
// file. On startup, existing records are recovered by replaying the log.
// Corrupted lines (e.g., from a mid-write crash) are silently skipped.
//
// Single-writer append model: no write queuing or conflict resolution needed
// since the ad review agent is a single-process service.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// JSONLWriter appends JSON objects as newline-delimited lines to a file.
// Thread-safe via mutex. Each Append call writes one complete line.
type JSONLWriter struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	logger *slog.Logger
	count  int // number of records written (including recovered)
}

// NewJSONLWriter opens (or creates) a JSONL file for appending.
// The parent directory is created if it doesn't exist.
func NewJSONLWriter(path string, logger *slog.Logger) (*JSONLWriter, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating directory %s: %w", dir, err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening JSONL file %s: %w", path, err)
	}

	return &JSONLWriter{
		file:   file,
		path:   path,
		logger: logger,
	}, nil
}

// Append serializes v as JSON and writes it as a single line.
// Errors are logged but do not propagate — persistence is best-effort
// and must never block the review pipeline.
func (w *JSONLWriter) Append(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		if w.logger != nil {
			w.logger.Error("JSONL marshal failed", slog.String("path", w.path), slog.String("error", err.Error()))
		}
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Append newline to form a complete JSONL line.
	data = append(data, '\n')
	if _, err := w.file.Write(data); err != nil {
		if w.logger != nil {
			w.logger.Error("JSONL write failed", slog.String("path", w.path), slog.String("error", err.Error()))
		}
		return
	}
	w.count++
}

// Flush ensures all buffered data is written to the underlying storage.
// Called during graceful shutdown.
func (w *JSONLWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Sync()
}

// Close flushes and closes the JSONL file.
func (w *JSONLWriter) Close() error {
	if err := w.Flush(); err != nil {
		return err
	}
	return w.file.Close()
}

// Path returns the file path for display/logging.
func (w *JSONLWriter) Path() string { return w.path }

// Count returns the total number of records (written + recovered).
func (w *JSONLWriter) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

// SetCount sets the internal counter (used after recovery to reflect total).
func (w *JSONLWriter) SetCount(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.count = n
}

// ReadJSONL reads a JSONL file and deserializes each line into type T.
// Corrupted or unparseable lines are skipped (crash safety).
// Returns (records, skippedLines, error).
// If the file doesn't exist, returns (nil, 0, nil) — not an error.
func ReadJSONL[T any](path string) ([]T, int, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("opening JSONL file %s: %w", path, err)
	}
	defer file.Close()

	var (
		records []T
		skipped int
	)

	scanner := bufio.NewScanner(file)
	// Increase buffer size for potentially large records (e.g., review results with violations).
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var record T
		if err := json.Unmarshal(line, &record); err != nil {
			skipped++
			continue
		}
		records = append(records, record)
	}

	if err := scanner.Err(); err != nil {
		return records, skipped, fmt.Errorf("reading JSONL file %s: %w", path, err)
	}

	return records, skipped, nil
}
