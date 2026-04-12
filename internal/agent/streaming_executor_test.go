package agent

import (
	"context"
	"testing"
	"time"

	"github.com/BillBeam/adguard-agent/internal/agent/mock"
	"github.com/BillBeam/adguard-agent/internal/types"
)

func TestStreamingExecutor_SingleTool(t *testing.T) {
	executor := mock.NewToolExecutor()
	se := NewStreamingToolExecutor(context.Background(), executor, nil, nil, nil, testLogger())

	se.AddTool("call_001", "analyze_content", `{"headline":"test","body":"body","category":"healthcare"}`)

	results := se.CollectResults(context.Background())
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ToolCallID != "call_001" {
		t.Errorf("ToolCallID = %q, want call_001", results[0].ToolCallID)
	}
	if results[0].Role != types.RoleTool {
		t.Errorf("Role = %q, want tool", results[0].Role)
	}
}

func TestStreamingExecutor_MultipleConcurrent(t *testing.T) {
	executor := mock.NewToolExecutor()
	se := NewStreamingToolExecutor(context.Background(), executor, nil, nil, nil, testLogger())

	// Add 3 tools — all concurrent-safe by default.
	se.AddTool("c1", "analyze_content", `{"headline":"t1","body":"b1","category":"healthcare"}`)
	se.AddTool("c2", "match_policies", `{"region":"US","category":"healthcare"}`)
	se.AddTool("c3", "check_landing_page", `{"url":"https://example.com"}`)

	results := se.CollectResults(context.Background())
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// Results should be in submission order regardless of completion order.
	if results[0].ToolCallID != "c1" {
		t.Errorf("result[0].ToolCallID = %q, want c1", results[0].ToolCallID)
	}
	if results[1].ToolCallID != "c2" {
		t.Errorf("result[1].ToolCallID = %q, want c2", results[1].ToolCallID)
	}
	if results[2].ToolCallID != "c3" {
		t.Errorf("result[2].ToolCallID = %q, want c3", results[2].ToolCallID)
	}
}

func TestStreamingExecutor_NonConcurrentBlocks(t *testing.T) {
	executor := mock.NewToolExecutor()

	// Use a checker that marks "analyze_content" as non-concurrent.
	checker := &mockConcurrencyChecker{
		nonConcurrent: map[string]bool{"analyze_content": true},
	}
	se := NewStreamingToolExecutor(context.Background(), executor, checker, nil, nil, testLogger())

	se.AddTool("c1", "analyze_content", `{"headline":"t1","body":"b1","category":"healthcare"}`)
	se.AddTool("c2", "match_policies", `{"region":"US","category":"healthcare"}`)

	results := se.CollectResults(context.Background())
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Both should complete successfully even with blocking.
	if results[0].ToolCallID != "c1" || results[1].ToolCallID != "c2" {
		t.Error("results not in submission order")
	}
}

func TestStreamingExecutor_OrderPreserved(t *testing.T) {
	executor := mock.NewToolExecutor()
	se := NewStreamingToolExecutor(context.Background(), executor, nil, nil, nil, testLogger())

	// Add tools in specific order.
	for i := 0; i < 5; i++ {
		id := "call_" + string(rune('a'+i))
		se.AddTool(id, "analyze_content", `{"headline":"test","body":"body","category":"healthcare"}`)
	}

	results := se.CollectResults(context.Background())
	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}
	for i, r := range results {
		expected := "call_" + string(rune('a'+i))
		if r.ToolCallID != expected {
			t.Errorf("result[%d].ToolCallID = %q, want %q", i, r.ToolCallID, expected)
		}
	}
}

func TestStreamingExecutor_ContextCancellation(t *testing.T) {
	executor := mock.NewToolExecutor()
	se := NewStreamingToolExecutor(context.Background(), executor, nil, nil, nil, testLogger())

	se.AddTool("c1", "analyze_content", `{"headline":"test","body":"body","category":"healthcare"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results := se.CollectResults(ctx)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

type mockConcurrencyChecker struct {
	nonConcurrent map[string]bool
}

func (m *mockConcurrencyChecker) IsConcurrencySafe(name string) bool {
	return !m.nonConcurrent[name]
}
