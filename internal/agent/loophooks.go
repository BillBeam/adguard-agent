package agent

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Hook execution helpers — run hooks with defer+recover protection.
// Same defensive pattern as HookChain.PostReview: a panicking hook
// cannot crash the agentic loop.

// runPreToolHooks runs all PreToolHooks for one tool call.
// Returns the first error (which blocks the tool), or nil.
func runPreToolHooks(hooks []PreToolHook, toolName string, args []byte, logger *slog.Logger) error {
	for _, hook := range hooks {
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("pre-tool hook panicked: %v", r)
					logger.Error("pre-tool hook panic", slog.String("tool", toolName), slog.String("panic", fmt.Sprint(r)))
				}
			}()
			err = hook.PreToolExec(toolName, args)
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// runPostToolHooks runs all PostToolHooks for one tool call. Informational only.
func runPostToolHooks(hooks []PostToolHook, toolName, result string, toolErr error, logger *slog.Logger) {
	for _, hook := range hooks {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("post-tool hook panic", slog.String("tool", toolName), slog.String("panic", fmt.Sprint(r)))
				}
			}()
			hook.PostToolExec(toolName, result, toolErr)
		}()
	}
}

// runStopHooks runs all StopHooks before the loop returns. Errors are logged, not propagated.
func runStopHooks(hooks []StopHook, state *State, reason ExitReason, logger *slog.Logger) {
	for _, hook := range hooks {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("stop hook panic", slog.String("panic", fmt.Sprint(r)))
				}
			}()
			if err := hook.BeforeStop(state, reason); err != nil {
				logger.Warn("stop hook error", slog.String("error", err.Error()))
			}
		}()
	}
}

// --- Concrete Hook implementations ---

// ToolPermissionHook implements PreToolHook.
// Restricts which tools can be called in a given pipeline.
// Example: fast pipeline cannot call lookup_history.
type ToolPermissionHook struct {
	allowed map[string]bool // tool_name -> allowed
	logger  *slog.Logger
}

// NewToolPermissionHook creates a hook that blocks tools not in the allowed set.
// Pass tool names that ARE allowed; all others are blocked.
func NewToolPermissionHook(allowedTools []string, logger *slog.Logger) *ToolPermissionHook {
	allowed := make(map[string]bool, len(allowedTools))
	for _, name := range allowedTools {
		allowed[name] = true
	}
	return &ToolPermissionHook{allowed: allowed, logger: logger}
}

func (h *ToolPermissionHook) PreToolExec(toolName string, _ []byte) error {
	if len(h.allowed) == 0 {
		return nil // empty config = allow all
	}
	if !h.allowed[toolName] {
		h.logger.Info("tool blocked by permission hook", slog.String("tool", toolName))
		return fmt.Errorf("tool %q not permitted in this pipeline", toolName)
	}
	return nil
}

// AuditHook implements both PreToolHook and PostToolHook.
// Records all tool invocations for audit trail.
type AuditHook struct {
	mu      sync.Mutex
	entries []AuditEntry
	logger  *slog.Logger
}

// AuditEntry records a single tool execution event.
type AuditEntry struct {
	ToolName  string    `json:"tool_name"`
	Phase     string    `json:"phase"` // "pre" or "post"
	Duration  time.Duration `json:"duration,omitempty"`
	HasError  bool      `json:"has_error"`
	Timestamp time.Time `json:"timestamp"`
}

// NewAuditHook creates an audit trail hook.
func NewAuditHook(logger *slog.Logger) *AuditHook {
	return &AuditHook{
		entries: make([]AuditEntry, 0, 32),
		logger:  logger,
	}
}

func (h *AuditHook) PreToolExec(toolName string, _ []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, AuditEntry{
		ToolName:  toolName,
		Phase:     "pre",
		Timestamp: time.Now(),
	})
	return nil // audit never blocks
}

func (h *AuditHook) PostToolExec(toolName string, result string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, AuditEntry{
		ToolName:  toolName,
		Phase:     "post",
		HasError:  err != nil || strings.HasPrefix(strings.TrimSpace(result), `{"error":`),
		Timestamp: time.Now(),
	})
}

// Entries returns a copy of the audit trail.
func (h *AuditHook) Entries() []AuditEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]AuditEntry, len(h.entries))
	copy(cp, h.entries)
	return cp
}

// CircuitBreakerHook implements both PreToolHook and PostToolHook.
// Counts consecutive tool failures; trips the breaker after threshold.
// When tripped, PreToolExec blocks all further tool calls.
type CircuitBreakerHook struct {
	mu               sync.Mutex
	consecutiveFails int
	threshold        int
	tripped          bool
	logger           *slog.Logger
}

// NewCircuitBreakerHook creates a circuit breaker that trips after threshold consecutive failures.
func NewCircuitBreakerHook(threshold int, logger *slog.Logger) *CircuitBreakerHook {
	return &CircuitBreakerHook{threshold: threshold, logger: logger}
}

func (h *CircuitBreakerHook) PreToolExec(toolName string, _ []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tripped {
		return fmt.Errorf("circuit breaker tripped: %d consecutive tool failures", h.consecutiveFails)
	}
	return nil
}

func (h *CircuitBreakerHook) PostToolExec(_ string, result string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Only count actual execution errors, not input validation failures.
	// LLM frequently sends malformed arguments on the first attempt and self-corrects —
	// this is normal behavior that should not trip the breaker.
	trimmed := strings.TrimSpace(result)
	if err != nil {
		h.consecutiveFails++
	} else if strings.HasPrefix(trimmed, `{"error":`) && !strings.Contains(trimmed, "invalid input") {
		h.consecutiveFails++
	} else {
		h.consecutiveFails = 0
	}

	if h.consecutiveFails >= h.threshold && !h.tripped {
		h.tripped = true
		h.logger.Warn("circuit breaker tripped", slog.Int("failures", h.consecutiveFails))
	}
}

// Reset clears the circuit breaker state. Called between reviews to prevent
// cross-review failure accumulation.
func (h *CircuitBreakerHook) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.consecutiveFails = 0
	h.tripped = false
}

// IsTripped returns whether the circuit breaker has been tripped.
func (h *CircuitBreakerHook) IsTripped() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tripped
}

// ResultValidationHook implements StopHook.
// Validates that the final ReviewResult has required fields.
type ResultValidationHook struct {
	logger *slog.Logger
}

// NewResultValidationHook creates a result validation hook.
func NewResultValidationHook(logger *slog.Logger) *ResultValidationHook {
	return &ResultValidationHook{logger: logger}
}

func (h *ResultValidationHook) BeforeStop(state *State, reason ExitReason) error {
	if reason != ExitCompleted {
		return nil // only validate successful completions
	}
	if state.PartialResult == nil {
		h.logger.Warn("stop validation: no partial result")
		return nil
	}
	if len(state.PartialResult.AgentTrace) == 0 {
		h.logger.Warn("stop validation: empty agent trace")
	}
	return nil
}

// FinalAuditHook implements StopHook.
// Emits a structured final audit log entry.
type FinalAuditHook struct {
	logger *slog.Logger
}

// NewFinalAuditHook creates a final audit hook.
func NewFinalAuditHook(logger *slog.Logger) *FinalAuditHook {
	return &FinalAuditHook{logger: logger}
}

func (h *FinalAuditHook) BeforeStop(state *State, reason ExitReason) error {
	h.logger.Info("review audit",
		slog.String("ad_id", state.AdContent.ID),
		slog.String("exit_reason", string(reason)),
		slog.Int("turns", state.TurnCount),
		slog.Int("trace_entries", len(state.PartialResult.AgentTrace)),
		slog.Duration("duration", time.Since(state.StartedAt)),
	)
	return nil
}
