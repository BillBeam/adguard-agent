package agent

import "time"

// EventType defines the types of events emitted by the agentic loop.
// These replace AsyncGenerator yield in the Go implementation.
type EventType string

const (
	EventTurnStarted       EventType = "turn_started"
	EventTurnCompleted     EventType = "turn_completed"
	EventAPICallStarted    EventType = "api_call_started"
	EventToolCallStarted   EventType = "tool_call_started"
	EventToolCallCompleted EventType = "tool_call_completed"
	EventRecoveryAttempt   EventType = "recovery_attempt"
	EventTombstone         EventType = "tombstone" // message invalidation marker

	// Phase 3: Context Management events.
	EventCompactStarted   EventType = "compact_started"
	EventCompactCompleted EventType = "compact_completed"
	EventBudgetWarning    EventType = "budget_warning"
)

// StreamEvent is an event emitted during the agentic loop execution.
// Sent through a channel, replacing AsyncGenerator's yield pattern.
type StreamEvent struct {
	Type      EventType `json:"type"`
	State     LoopState `json:"state"`
	TurnCount int       `json:"turn_count"`
	Timestamp time.Time `json:"timestamp"`
	Detail    string    `json:"detail,omitempty"`
}
