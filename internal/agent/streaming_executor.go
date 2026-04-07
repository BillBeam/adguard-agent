// StreamingToolExecutor enables tool execution during LLM streaming responses.
//
// In non-streaming mode, the agent loop waits for the full LLM response before
// executing tools. StreamingToolExecutor starts tool execution as soon as each
// tool_use block's parameters are complete — while the LLM is still generating.
//
// Concurrency rules (matching production agent patterns):
//   - No executing tools → start immediately
//   - Tool is concurrent-safe AND all executing tools are concurrent-safe → start
//   - Non-concurrent tool encountered → block queue until all executing tools finish
//
// Go channel + goroutine is the natural equivalent of TypeScript's AsyncGenerator
// pattern: goroutines for parallel execution, channels for result collection,
// and sync primitives for ordering guarantees.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/BillBeam/adguard-agent/internal/types"
)

type toolStatus int

const (
	statusQueued    toolStatus = iota
	statusExecuting
	statusCompleted
)

type trackedTool struct {
	id              string
	name            string
	arguments       string // accumulated JSON parameters
	status          toolStatus
	concurrencySafe bool
	result          types.Message
	done            chan struct{} // closed when execution completes
}

// ConcurrencyChecker determines if a tool is safe for parallel execution.
type ConcurrencyChecker interface {
	IsConcurrencySafe(toolName string) bool
}

// StreamingToolExecutor manages tool execution during LLM streaming.
type StreamingToolExecutor struct {
	mu               sync.Mutex
	tools            []*trackedTool
	executor         ToolExecutor
	concurrencyCheck ConcurrencyChecker // nil = assume all concurrent-safe
	logger           *slog.Logger
}

// NewStreamingToolExecutor creates an executor for streaming tool dispatch.
func NewStreamingToolExecutor(executor ToolExecutor, cc ConcurrencyChecker, logger *slog.Logger) *StreamingToolExecutor {
	return &StreamingToolExecutor{
		executor:         executor,
		concurrencyCheck: cc,
		logger:           logger,
	}
}

// AddTool queues a tool for execution. Called when a tool_use block's
// parameters are fully accumulated (new index appears or finish_reason arrives).
func (s *StreamingToolExecutor) AddTool(id, name, arguments string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Determine concurrency safety. Default to concurrent-safe (all ad review
	// tools are read-only analyzers). Override via ConcurrencyChecker if available.
	isSafe := true
	if s.concurrencyCheck != nil {
		isSafe = s.concurrencyCheck.IsConcurrencySafe(name)
	}

	t := &trackedTool{
		id:              id,
		name:            name,
		arguments:       arguments,
		status:          statusQueued,
		concurrencySafe: isSafe,
		done:            make(chan struct{}),
	}
	s.tools = append(s.tools, t)

	s.logger.Debug("streaming executor: tool queued",
		slog.String("tool", name),
		slog.Bool("concurrent_safe", isSafe),
		slog.Int("queue_size", len(s.tools)),
	)

	s.processQueueLocked()
}

// processQueueLocked iterates tools in order and starts eligible ones.
// Must be called with s.mu held.
func (s *StreamingToolExecutor) processQueueLocked() {
	for _, t := range s.tools {
		if t.status != statusQueued {
			continue
		}
		if s.canExecuteLocked(t.concurrencySafe) {
			t.status = statusExecuting
			go s.executeTool(t)
		} else if !t.concurrencySafe {
			// Non-concurrent tool must wait — stop scanning to preserve order.
			break
		}
		// Concurrent-safe tool that can't run yet: skip, check next.
	}
}

// canExecuteLocked checks if a tool can start now.
// Must be called with s.mu held.
func (s *StreamingToolExecutor) canExecuteLocked(isConcurrencySafe bool) bool {
	hasExecuting := false
	allExecutingSafe := true
	for _, t := range s.tools {
		if t.status == statusExecuting {
			hasExecuting = true
			if !t.concurrencySafe {
				allExecutingSafe = false
				break
			}
		}
	}
	// No executing tools → always allowed.
	if !hasExecuting {
		return true
	}
	// Concurrent-safe tool + all executing are concurrent-safe → allowed.
	return isConcurrencySafe && allExecutingSafe
}

// executeTool runs a single tool in a goroutine.
func (s *StreamingToolExecutor) executeTool(t *trackedTool) {
	defer close(t.done)

	// Build a ToolCall to pass to the standard executor.
	tc := types.ToolCall{
		ID:   t.id,
		Type: "function",
		Function: types.ToolCallFunction{
			Name:      t.name,
			Arguments: json.RawMessage(t.arguments),
		},
	}

	results, err := s.executor.Execute(context.Background(), []types.ToolCall{tc})

	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil || len(results) == 0 {
		t.result = types.Message{
			Role:       types.RoleTool,
			Content:    types.NewTextContent(fmt.Sprintf(`{"error":%q}`, err)),
			ToolCallID: t.id,
		}
	} else {
		t.result = results[0]
	}
	t.status = statusCompleted

	s.logger.Debug("streaming executor: tool completed",
		slog.String("tool", t.name),
	)

	// Re-check queue — completing this tool may unblock waiting tools.
	s.processQueueLocked()
}

// CollectResults waits for all tools to complete and returns results in
// submission order (matching LLM output order, not completion order).
func (s *StreamingToolExecutor) CollectResults(ctx context.Context) []types.Message {
	s.mu.Lock()
	tools := make([]*trackedTool, len(s.tools))
	copy(tools, s.tools)
	s.mu.Unlock()

	results := make([]types.Message, 0, len(tools))
	for _, t := range tools {
		select {
		case <-t.done:
			results = append(results, t.result)
		case <-ctx.Done():
			// Context cancelled — return what we have.
			results = append(results, types.Message{
				Role:       types.RoleTool,
				Content:    types.NewTextContent(`{"error":"context cancelled during tool execution"}`),
				ToolCallID: t.id,
			})
		}
	}
	return results
}

// ToolCount returns the number of tools in the queue.
func (s *StreamingToolExecutor) ToolCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tools)
}

