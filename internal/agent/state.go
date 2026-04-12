// Package agent implements the Agentic Loop — the core review lifecycle
// state machine that drives ad content through analysis to decision.
//
// Design pattern: while(true) + State + TransitionReason. Each loop
// iteration corresponds to one API call → response parsing → tool
// execution → continue/exit cycle.
package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// --- LoopState: review lifecycle states ---

// LoopState represents the current stage in the ad review lifecycle.
type LoopState string

const (
	StatePending   LoopState = "PENDING"    // Ad submitted, awaiting processing
	StateAnalyzing LoopState = "ANALYZING"  // Inside the agent loop: tool-call analysis
	StateJudging   LoopState = "JUDGING"    // Analysis complete, LLM producing final decision
	StateDecided   LoopState = "DECIDED"    // Review complete, ReviewResult produced
	StateError     LoopState = "ERROR"      // Unrecoverable error
	StateCancelled LoopState = "CANCELLED"  // Cancelled (context.Done)
)

// IsTerminal returns true if the state is a terminal state (no further transitions).
func (s LoopState) IsTerminal() bool {
	return s == StateDecided || s == StateError || s == StateCancelled
}

// --- TransitionReason: reasons for state transitions ---

// TransitionReason describes why a state transition occurred.
type TransitionReason string

// Normal progression.
const (
	TransitionInitialized            TransitionReason = "initialized"              // Loop initialized
	TransitionNextTurn               TransitionReason = "next_turn"                // Tool execution done, continue next turn
	TransitionConfidenceInsufficient TransitionReason = "confidence_insufficient"  // Confidence too low, escalate to manual review
)

// Recovery.
const (
	TransitionMaxOutputEscalate TransitionReason = "max_output_tokens_escalate" // Output truncated, escalate token limit
	TransitionMaxOutputRecovery TransitionReason = "max_output_tokens_recovery" // Output truncated, inject recovery message
	TransitionAutoCompact     TransitionReason = "auto_compact"      // proactive AutoCompact compression
	TransitionReactiveCompact TransitionReason = "reactive_compact"  // reactive compression after prompt_too_long
	TransitionBudgetExhausted TransitionReason = "budget_exhausted"  // token budget limit reached
)

// Termination.
const (
	TransitionCompleted     TransitionReason = "completed"       // Normal completion
	TransitionMaxTurns      TransitionReason = "max_turns"       // Turn limit reached
	TransitionModelError    TransitionReason = "model_error"     // Unrecoverable API error
	TransitionAborted       TransitionReason = "aborted"         // Context cancelled
	TransitionPromptTooLong TransitionReason = "prompt_too_long" // Prompt too long and recovery failed
)

// --- TransitionRecord: state transition record ---

// TransitionRecord captures a single state transition for the audit trail.
type TransitionRecord struct {
	From      LoopState        `json:"from"`
	To        LoopState        `json:"to"`
	Reason    TransitionReason `json:"reason"`
	TurnCount int              `json:"turn_count"`
	Timestamp time.Time        `json:"timestamp"`
	Detail    string           `json:"detail,omitempty"`
}

// --- PartialReviewResult: accumulated results during the review loop ---

// PartialReviewResult accumulates intermediate findings during the review loop.
type PartialReviewResult struct {
	Violations []types.PolicyViolation
	AgentTrace []string
}

// --- State: cross-iteration loop state ---

// State is the core data structure passed across loop iterations.
type State struct {
	// Messages is the append-only conversation history.
	Messages []types.Message

	// Loop control.
	LoopState LoopState
	TurnCount int

	// Recovery state (maps to maxOutputTokensRecoveryCount etc.).
	MaxOutputRecoveryCount int
	MaxTokensOverride      *int // nil = use default; set to 64k after escalation
	HasAttemptedCompact    bool // Phase 3 expansion point

	// Review-specific state.
	AdContent     *types.AdContent
	PartialResult *PartialReviewResult
	AgentRole     string // role of the agent ("content", "policy", "region", "single", "appeal")

	// Audit trail: complete history of all state transitions.
	TransitionLog []TransitionRecord

	// Timing.
	StartedAt time.Time

	// StreamMetrics captures streaming tool execution performance.
	// nil when non-streaming mode is used.
	StreamMetrics *StreamMetrics
}

// StreamMetrics records how tools were dispatched during streaming.
type StreamMetrics struct {
	StreamDuration  time.Duration `json:"stream_duration"`
	CollectWait     time.Duration `json:"collect_wait"`
	ToolsDispatched int           `json:"tools_dispatched"`
}

// NewState creates the initial state for reviewing an ad.
func NewState(ad *types.AdContent) *State {
	return &State{
		LoopState: StatePending,
		AdContent: ad,
		PartialResult: &PartialReviewResult{
			AgentTrace: make([]string, 0, 16),
		},
		TransitionLog: make([]TransitionRecord, 0, 16),
		StartedAt:     time.Now(),
	}
}

// Transition executes a state transition and records it to the audit log.
func (s *State) Transition(to LoopState, reason TransitionReason, detail string) {
	record := TransitionRecord{
		From:      s.LoopState,
		To:        to,
		Reason:    reason,
		TurnCount: s.TurnCount,
		Timestamp: time.Now(),
		Detail:    detail,
	}
	s.LoopState = to
	s.TransitionLog = append(s.TransitionLog, record)
}

// AppendMessage appends a message to the conversation history (append-only).
func (s *State) AppendMessage(msg types.Message) {
	s.Messages = append(s.Messages, msg)
}

// AppendTrace records an agent action to the audit trace.
func (s *State) AppendTrace(action string) {
	s.PartialResult.AgentTrace = append(s.PartialResult.AgentTrace, action)
}

// FormatTransitionLog returns a human-readable summary of state transitions.
// Example: "turn_0(initialized) → turn_0(tool_call:analyze_content) → turn_1(completed:REJECTED)"
func (s *State) FormatTransitionLog() string {
	if len(s.TransitionLog) == 0 {
		return "(no transitions)"
	}
	var b strings.Builder
	for i, t := range s.TransitionLog {
		if i > 0 {
			b.WriteString(" → ")
		}
		fmt.Fprintf(&b, "turn_%d(%s", t.TurnCount, t.Reason)
		if t.Detail != "" {
			b.WriteString(":")
			b.WriteString(t.Detail)
		}
		b.WriteString(")")
	}
	return b.String()
}
