package agent

import (
	"fmt"
	"log/slog"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// HookChain composes multiple PostReviewHook implementations into a chain.
// Hooks are invoked in registration order. A panic in one hook is recovered
// and logged — subsequent hooks still execute.
//
// HookChain itself implements PostReviewHook, so it can be passed directly
// to NewReviewEngine's postReviewHook parameter (backward compatible).
type HookChain struct {
	hooks  []PostReviewHook
	logger *slog.Logger
}

// NewHookChain creates an empty hook chain.
func NewHookChain(logger *slog.Logger) *HookChain {
	return &HookChain{
		hooks:  make([]PostReviewHook, 0, 4),
		logger: logger,
	}
}

// Add appends a hook to the chain. Returns the chain for fluent calls.
func (hc *HookChain) Add(hook PostReviewHook) *HookChain {
	hc.hooks = append(hc.hooks, hook)
	return hc
}

// PostReview implements PostReviewHook. Invokes all registered hooks in order.
// Each hook runs in a deferred-recover wrapper so a panicking hook cannot
// break the chain.
func (hc *HookChain) PostReview(result types.ReviewResult, advertiserID, region, category, pipeline string) {
	for i, hook := range hc.hooks {
		func(idx int, h PostReviewHook) {
			defer func() {
				if r := recover(); r != nil {
					hc.logger.Error("hook panicked",
						slog.Int("index", idx),
						slog.String("panic", fmt.Sprint(r)),
					)
				}
			}()
			h.PostReview(result, advertiserID, region, category, pipeline)
		}(i, hook)
	}
}

// --- Phase 5 Hook interface skeletons (defined now, implemented later) ---

// PreToolHook is called before tool execution.
// Phase 5 use cases: permission checks, input auditing.
type PreToolHook interface {
	PreToolExec(toolName string, args []byte) error // return error to block execution
}

// PostToolHook is called after tool execution.
// Phase 5 use cases: result auditing, metrics recording.
type PostToolHook interface {
	PostToolExec(toolName string, result string, err error)
}

// StopHook is called before the loop terminates.
// Phase 5 use cases: final validation, resource cleanup.
type StopHook interface {
	BeforeStop(state *State, reason ExitReason) error
}
