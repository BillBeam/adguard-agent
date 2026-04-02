package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BillBeam/adguard-agent/internal/types"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// fakeTool is a configurable test tool.
type fakeTool struct {
	BaseTool
	name     string
	execFn   func(ctx context.Context, args json.RawMessage) (string, error)
	validFn  func(args json.RawMessage) error
	schema   json.RawMessage
	execTime time.Duration
}

func (f *fakeTool) Name() string              { return f.name }
func (f *fakeTool) Description() string        { return "test tool" }
func (f *fakeTool) InputSchema() json.RawMessage { return f.schema }
func (f *fakeTool) ValidateInput(args json.RawMessage) error {
	if f.validFn != nil {
		return f.validFn(args)
	}
	return nil
}
func (f *fakeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if f.execTime > 0 {
		time.Sleep(f.execTime)
	}
	if f.execFn != nil {
		return f.execFn(ctx, args)
	}
	return `{"result":"ok"}`, nil
}

func TestExecutor_ParallelExecution(t *testing.T) {
	var execCount atomic.Int32
	reg := NewRegistry()
	for i := range 2 {
		name := fmt.Sprintf("tool_%d", i)
		reg.Register(&fakeTool{
			BaseTool: ReviewToolBase(), // concurrent safe
			name:     name,
			schema:   json.RawMessage(`{"type":"object"}`),
			execTime: 100 * time.Millisecond,
			execFn: func(_ context.Context, _ json.RawMessage) (string, error) {
				execCount.Add(1)
				return `{"ok":true}`, nil
			},
		})
	}

	executor := NewExecutor(reg, testLogger())
	calls := []types.ToolCall{
		{ID: "c1", Type: "function", Function: types.ToolCallFunction{Name: "tool_0", Arguments: json.RawMessage(`{}`)}},
		{ID: "c2", Type: "function", Function: types.ToolCallFunction{Name: "tool_1", Arguments: json.RawMessage(`{}`)}},
	}

	start := time.Now()
	results, err := executor.Execute(context.Background(), calls)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if execCount.Load() != 2 {
		t.Errorf("expected 2 executions, got %d", execCount.Load())
	}
	// Both tools sleep 100ms. If parallel, total should be ~100ms, not ~200ms.
	if elapsed > 180*time.Millisecond {
		t.Errorf("execution took %v, expected parallel (~100ms)", elapsed)
	}
	t.Logf("Parallel execution took %v", elapsed)
}

func TestExecutor_UnknownTool(t *testing.T) {
	reg := NewRegistry()
	executor := NewExecutor(reg, testLogger())

	calls := []types.ToolCall{
		{ID: "c1", Type: "function", Function: types.ToolCallFunction{Name: "nonexistent", Arguments: json.RawMessage(`{}`)}},
	}

	results, err := executor.Execute(context.Background(), calls)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	content := results[0].Content.String()
	if !strings.Contains(content, "unknown tool") {
		t.Errorf("expected error about unknown tool, got: %s", content)
	}
	if results[0].ToolCallID != "c1" {
		t.Errorf("ToolCallID = %s, want c1", results[0].ToolCallID)
	}
}

func TestExecutor_SingleToolFailure(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeTool{
		BaseTool: ReviewToolBase(),
		name:     "good_tool",
		schema:   json.RawMessage(`{"type":"object"}`),
		execFn:   func(_ context.Context, _ json.RawMessage) (string, error) { return `{"ok":true}`, nil },
	})
	reg.Register(&fakeTool{
		BaseTool: ReviewToolBase(),
		name:     "bad_tool",
		schema:   json.RawMessage(`{"type":"object"}`),
		execFn:   func(_ context.Context, _ json.RawMessage) (string, error) { return "", fmt.Errorf("boom") },
	})

	executor := NewExecutor(reg, testLogger())
	calls := []types.ToolCall{
		{ID: "c1", Type: "function", Function: types.ToolCallFunction{Name: "good_tool", Arguments: json.RawMessage(`{}`)}},
		{ID: "c2", Type: "function", Function: types.ToolCallFunction{Name: "bad_tool", Arguments: json.RawMessage(`{}`)}},
	}

	results, err := executor.Execute(context.Background(), calls)
	if err != nil {
		t.Fatalf("Execute should not return error: %v", err)
	}

	// Good tool should succeed.
	if strings.Contains(results[0].Content.String(), "error") {
		t.Errorf("good_tool should succeed, got: %s", results[0].Content.String())
	}
	// Bad tool should have error message.
	if !strings.Contains(results[1].Content.String(), "boom") {
		t.Errorf("bad_tool should contain error, got: %s", results[1].Content.String())
	}
}

func TestExecutor_ValidationFailure(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeTool{
		BaseTool: ReviewToolBase(),
		name:     "strict_tool",
		schema:   json.RawMessage(`{"type":"object"}`),
		validFn: func(args json.RawMessage) error {
			return fmt.Errorf("missing required field 'name'")
		},
	})

	executor := NewExecutor(reg, testLogger())
	calls := []types.ToolCall{
		{ID: "c1", Type: "function", Function: types.ToolCallFunction{Name: "strict_tool", Arguments: json.RawMessage(`{}`)}},
	}

	results, _ := executor.Execute(context.Background(), calls)
	if !strings.Contains(results[0].Content.String(), "invalid input") {
		t.Errorf("expected validation error, got: %s", results[0].Content.String())
	}
}

func TestExecutor_ResultTruncation(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeTool{
		BaseTool: BaseTool{concurrencySafe: true, readOnly: true, maxResultSize: 50},
		name:     "verbose_tool",
		schema:   json.RawMessage(`{"type":"object"}`),
		execFn: func(_ context.Context, _ json.RawMessage) (string, error) {
			return strings.Repeat("x", 200), nil
		},
	})

	executor := NewExecutor(reg, testLogger())
	calls := []types.ToolCall{
		{ID: "c1", Type: "function", Function: types.ToolCallFunction{Name: "verbose_tool", Arguments: json.RawMessage(`{}`)}},
	}

	results, _ := executor.Execute(context.Background(), calls)
	content := results[0].Content.String()
	if len(content) > 100 {
		t.Errorf("result should be truncated, got %d chars", len(content))
	}
	if !strings.Contains(content, "[truncated]") {
		t.Errorf("expected truncation marker, got: %s", content[:min(len(content), 80)])
	}
}

func TestExecutor_PostReview(t *testing.T) {
	reg := NewRegistry()
	hl := NewHistoryLookup(testLogger())
	reg.Register(hl)

	executor := NewExecutor(reg, testLogger())
	executor.PostReview(types.ReviewResult{
		AdID:     "test_001",
		Decision: types.DecisionRejected,
	}, "adv_1", "US", "healthcare", "standard")

	// Verify the record was added.
	result, _ := hl.Execute(context.Background(), json.RawMessage(`{"advertiser_id":"adv_1"}`))
	if !strings.Contains(result, `"total_history_records":1`) {
		t.Errorf("expected 1 history record, got: %s", result)
	}
}
