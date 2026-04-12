// StreamAccumulator processes SSE chunks from an OpenAI-compatible streaming
// response and accumulates them into a complete ChatCompletionResponse.
//
// Key behaviors:
//   - Text deltas: concatenated immediately (O(n) via strings.Builder)
//   - Tool call deltas: arguments accumulated per-index; when a new index
//     appears, the previous index's tool call is considered complete and
//     submitted to the StreamingToolExecutor for immediate execution
//   - JSON fragments are NOT parsed incrementally (O(n²) cost for repeated
//     parse attempts that fail 99% of the time); parsed once at completion
//   - finish_reason: captured from the final chunk; triggers submission of
//     the last accumulated tool call
package agent

import (
	"log/slog"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// accumulatedToolCall tracks one in-progress tool call during streaming.
type accumulatedToolCall struct {
	index     int
	id        string
	name      string
	arguments strings.Builder // JSON fragments accumulated across chunks
	submitted bool
	// JSON boundary detection state machine: tracks brace depth char-by-char; depth returning to 0 means JSON is complete.
	depth    int  // Brace/bracket nesting depth
	inString bool // Currently inside a JSON string?
	escaped  bool // Previous character was a backslash?
	complete bool // Depth went from >0 back to 0; JSON object is complete
}

// StreamAccumulator builds a complete API response from streaming chunks.
type StreamAccumulator struct {
	textContent strings.Builder
	toolCalls   map[int]*accumulatedToolCall
	maxIndex    int // highest tool call index seen so far (-1 = none)
	finishReason string
	usage       *types.Usage
	model       string

	executor *StreamingToolExecutor // receives completed tool calls
	logger   *slog.Logger
}

// NewStreamAccumulator creates an accumulator connected to a streaming executor.
func NewStreamAccumulator(executor *StreamingToolExecutor, logger *slog.Logger) *StreamAccumulator {
	return &StreamAccumulator{
		toolCalls: make(map[int]*accumulatedToolCall),
		maxIndex:  -1,
		executor:  executor,
		logger:    logger,
	}
}

// ProcessChunk handles one SSE chunk from the streaming response.
func (a *StreamAccumulator) ProcessChunk(chunk *types.ChatCompletionChunk) {
	if chunk.Model != "" {
		a.model = chunk.Model
	}
	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}

	for _, choice := range chunk.Choices {
		delta := choice.Delta

		// Text content: accumulate directly.
		if text := delta.Content.String(); text != "" {
			a.textContent.WriteString(text)
		}

		// Tool calls: accumulate per-index.
		for _, tc := range delta.ToolCalls {
			idx := tc.Index
			atc, exists := a.toolCalls[idx]
			if !exists {
				// New tool call index — submit all previous indexes.
				if idx > a.maxIndex && a.maxIndex >= 0 {
					a.submitUpTo(a.maxIndex)
				}

				atc = &accumulatedToolCall{index: idx}
				a.toolCalls[idx] = atc
				if idx > a.maxIndex {
					a.maxIndex = idx
				}
			}

			// Accumulate fields from delta.
			if tc.ID != "" {
				atc.id = tc.ID
			}
			if tc.Function.Name != "" {
				atc.name = tc.Function.Name
			}
			if len(tc.Function.Arguments) > 0 {
				frag := string(tc.Function.Arguments)
				atc.arguments.WriteString(frag)
				// Incremental JSON boundary detection: submit as soon as arguments are complete, don't wait for subsequent tools.
				atc.trackJSONBoundary(frag)
				if atc.complete && !atc.submitted {
					a.submitTool(idx)
				}
			}
		}

		// finish_reason: capture and submit last tool call.
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			a.finishReason = *choice.FinishReason
		}
	}
}

// Finalize ensures all accumulated tool calls are submitted.
// Must be called after the stream ends (io.EOF).
func (a *StreamAccumulator) Finalize() {
	if a.maxIndex >= 0 {
		a.submitUpTo(a.maxIndex)
	}
}

// submitUpTo submits all tool calls with index <= maxIdx that haven't been submitted yet.
func (a *StreamAccumulator) submitUpTo(maxIdx int) {
	for idx := 0; idx <= maxIdx; idx++ {
		a.submitTool(idx)
	}
}

// submitTool submits a single tool call. Skips if already submitted (idempotent).
func (a *StreamAccumulator) submitTool(idx int) {
	atc, ok := a.toolCalls[idx]
	if !ok || atc.submitted {
		return
	}
	atc.submitted = true

	if a.executor != nil {
		a.executor.AddTool(atc.id, atc.name, atc.arguments.String())
		a.logger.Debug("stream accumulator: tool call submitted",
			slog.Int("index", idx),
			slog.String("tool", atc.name),
		)
	}
}

// trackJSONBoundary scans JSON fragments to detect object/array boundary completion.
// Tracks brace depth, string context, and escape sequences character by character.
// When depth drops from positive back to 0, the JSON object is complete and can be submitted immediately.
func (atc *accumulatedToolCall) trackJSONBoundary(fragment string) {
	if atc.complete {
		return
	}
	for _, ch := range fragment {
		if atc.escaped {
			atc.escaped = false
			continue
		}
		if atc.inString {
			switch ch {
			case '\\':
				atc.escaped = true
			case '"':
				atc.inString = false
			}
			continue
		}
		switch ch {
		case '"':
			atc.inString = true
		case '{', '[':
			atc.depth++
		case '}', ']':
			atc.depth--
			if atc.depth == 0 {
				atc.complete = true
				return
			}
		}
	}
}

// FinishReason returns the captured finish_reason.
func (a *StreamAccumulator) FinishReason() string {
	return a.finishReason
}

// BuildResponse constructs a ChatCompletionResponse equivalent to what
// a non-streaming call would return. Used by the loop to branch on
// finish_reason identically to non-streaming mode.
func (a *StreamAccumulator) BuildResponse() *types.ChatCompletionResponse {
	msg := types.Message{
		Role: types.RoleAssistant,
	}

	// Set text content if any.
	if a.textContent.Len() > 0 {
		msg.Content = types.NewTextContent(a.textContent.String())
	}

	// Build tool calls from accumulated data.
	for idx := 0; idx <= a.maxIndex; idx++ {
		atc, ok := a.toolCalls[idx]
		if !ok {
			continue
		}
		msg.ToolCalls = append(msg.ToolCalls, types.ToolCall{
			ID:   atc.id,
			Type: "function",
			Function: types.ToolCallFunction{
				Name:      atc.name,
				Arguments: []byte(atc.arguments.String()),
			},
		})
	}

	return &types.ChatCompletionResponse{
		Model: a.model,
		Choices: []types.Choice{{
			Message:      msg,
			FinishReason: a.finishReason,
		}},
		Usage: a.usage,
	}
}
