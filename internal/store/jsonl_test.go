package store

import (
	"os"
	"path/filepath"
	"testing"
)

type jsonlTestRecord struct {
	ID    string `json:"id"`
	Value int    `json:"value"`
}

func TestJSONLWriter_AppendAndRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	w, err := NewJSONLWriter(path, nil)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}

	records := []jsonlTestRecord{
		{ID: "a", Value: 1},
		{ID: "b", Value: 2},
		{ID: "c", Value: 3},
	}
	for _, r := range records {
		w.Append(r)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, skipped, err := ReadJSONL[jsonlTestRecord](path)
	if err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	for i, r := range got {
		if r.ID != records[i].ID || r.Value != records[i].Value {
			t.Errorf("record[%d] = %+v, want %+v", i, r, records[i])
		}
	}
}

func TestJSONLWriter_CrashRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crash.jsonl")

	// Write 2 valid records + 1 corrupted line (simulating mid-write crash).
	w, _ := NewJSONLWriter(path, nil)
	w.Append(jsonlTestRecord{ID: "valid1", Value: 10})
	w.Append(jsonlTestRecord{ID: "valid2", Value: 20})
	w.Close()

	// Append a corrupt partial line.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	f.WriteString(`{"id":"broken","val`)
	f.Close()

	got, skipped, err := ReadJSONL[jsonlTestRecord](path)
	if err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (corrupted line)", skipped)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if got[0].ID != "valid1" || got[1].ID != "valid2" {
		t.Errorf("unexpected records: %+v", got)
	}
}

func TestJSONLWriter_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")

	// Create an empty file.
	os.WriteFile(path, nil, 0644)

	got, skipped, err := ReadJSONL[jsonlTestRecord](path)
	if err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	if skipped != 0 || len(got) != 0 {
		t.Errorf("expected empty result, got %d records, %d skipped", len(got), skipped)
	}
}

func TestReadJSONL_FileNotExist(t *testing.T) {
	got, skipped, err := ReadJSONL[jsonlTestRecord]("/nonexistent/path.jsonl")
	if err != nil {
		t.Fatalf("ReadJSONL: %v (expected nil for missing file)", err)
	}
	if skipped != 0 || len(got) != 0 {
		t.Errorf("expected empty result, got %d records, %d skipped", len(got), skipped)
	}
}

func TestJSONLWriter_Count(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "count.jsonl")

	w, _ := NewJSONLWriter(path, nil)
	if w.Count() != 0 {
		t.Errorf("initial count = %d, want 0", w.Count())
	}

	w.Append(jsonlTestRecord{ID: "a", Value: 1})
	w.Append(jsonlTestRecord{ID: "b", Value: 2})
	if w.Count() != 2 {
		t.Errorf("after 2 appends, count = %d, want 2", w.Count())
	}

	w.SetCount(10)
	if w.Count() != 10 {
		t.Errorf("after SetCount(10), count = %d, want 10", w.Count())
	}
	w.Close()
}

func TestJSONLWriter_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "test.jsonl")

	w, err := NewJSONLWriter(path, nil)
	if err != nil {
		t.Fatalf("NewJSONLWriter with nested dir: %v", err)
	}
	w.Append(jsonlTestRecord{ID: "x", Value: 42})
	w.Close()

	got, _, _ := ReadJSONL[jsonlTestRecord](path)
	if len(got) != 1 || got[0].ID != "x" {
		t.Errorf("expected 1 record with ID=x, got %+v", got)
	}
}
