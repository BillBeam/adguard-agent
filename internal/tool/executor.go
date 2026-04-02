package tool

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// Executor implements the agent.ToolExecutor interface using the tool registry.
// It resolves tool calls, validates input, executes tools (concurrently when safe),
// and constructs tool_result messages.
//
// Concurrency strategy:
//   - If ALL requested tools are IsConcurrencySafe: parallel via goroutines
//   - Otherwise: sequential, preserving order
//   - Individual tool failure produces error message for THAT tool only
//   - Unknown tool name produces error message (never panic)
//
// The Execute method always returns nil error. Individual tool failures are
// communicated as error messages in the returned []Message, consistent with
// the agentic loop's fail-closed error recovery in loop.go.
type Executor struct {
	registry *Registry
	logger   *slog.Logger
}

// NewExecutor creates a tool executor backed by the given registry.
func NewExecutor(registry *Registry, logger *slog.Logger) *Executor {
	return &Executor{registry: registry, logger: logger}
}

// Execute resolves and runs tool calls, returning tool_result messages.
// Satisfies agent.ToolExecutor via Go's implicit interface satisfaction.
func (e *Executor) Execute(ctx context.Context, toolCalls []types.ToolCall) ([]types.Message, error) {
	results := make([]types.Message, len(toolCalls))

	// Determine if all tools are concurrency-safe.
	allConcurrent := true
	for _, tc := range toolCalls {
		t, ok := e.registry.Get(tc.Function.Name)
		if !ok || !t.IsConcurrencySafe() {
			allConcurrent = false
			break
		}
	}

	if allConcurrent && len(toolCalls) > 1 {
		var wg sync.WaitGroup
		wg.Add(len(toolCalls))
		for i, tc := range toolCalls {
			go func(idx int, call types.ToolCall) {
				defer wg.Done()
				results[idx] = e.executeSingle(ctx, call)
			}(i, tc)
		}
		wg.Wait()
	} else {
		for i, tc := range toolCalls {
			results[i] = e.executeSingle(ctx, tc)
		}
	}
	return results, nil
}

// PostReview records a completed review result into the HistoryLookup tool.
// Called by ReviewEngine after each review completes.
// advertiserID, region, category provide the ad context for matching.
func (e *Executor) PostReview(result types.ReviewResult, advertiserID, region, category, _ string) {
	t, ok := e.registry.Get("lookup_history")
	if !ok {
		return
	}
	if hl, ok := t.(*HistoryLookup); ok {
		hl.AddRecord(result, advertiserID, region, category)
	}
}

// executeSingle handles a single tool call through the full pipeline:
// findTool → validateInput → execute → truncateIfNeeded → buildMessage.
func (e *Executor) executeSingle(ctx context.Context, tc types.ToolCall) types.Message {
	// 1. Find tool.
	t, ok := e.registry.Get(tc.Function.Name)
	if !ok {
		e.logger.Warn("unknown tool requested", slog.String("tool", tc.Function.Name))
		return toolErrorMessage(tc.ID, fmt.Sprintf("unknown tool %q", tc.Function.Name))
	}

	// 2. Validate input.
	if err := t.ValidateInput(tc.Function.Arguments); err != nil {
		e.logger.Warn("tool input validation failed",
			slog.String("tool", tc.Function.Name),
			slog.String("error", err.Error()),
		)
		return toolErrorMessage(tc.ID, fmt.Sprintf("invalid input: %s", err.Error()))
	}

	// 3. Execute.
	e.logger.Debug("executing tool", slog.String("tool", tc.Function.Name))
	output, err := t.Execute(ctx, tc.Function.Arguments)
	if err != nil {
		e.logger.Error("tool execution failed",
			slog.String("tool", tc.Function.Name),
			slog.String("error", err.Error()),
		)
		return toolErrorMessage(tc.ID, err.Error())
	}

	// 4. Truncate if exceeds MaxResultSize.
	if maxSize := t.MaxResultSize(); maxSize > 0 && len(output) > maxSize {
		output = output[:maxSize] + "\n...[truncated]"
		e.logger.Warn("tool result truncated",
			slog.String("tool", tc.Function.Name),
			slog.Int("max_size", maxSize),
		)
	}

	// 5. Build success message.
	return types.Message{
		Role:       types.RoleTool,
		Content:    types.NewTextContent(output),
		ToolCallID: tc.ID,
	}
}

// toolErrorMessage builds a tool_result message for an error case.
func toolErrorMessage(toolCallID, errMsg string) types.Message {
	return types.Message{
		Role:       types.RoleTool,
		Content:    types.NewTextContent(fmt.Sprintf(`{"error":%q}`, errMsg)),
		ToolCallID: toolCallID,
	}
}
