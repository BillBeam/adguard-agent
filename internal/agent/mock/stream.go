package mock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// NewStreamReaderFromResponse creates a StreamReader that delivers the given
// response as a sequence of SSE chunks, simulating a real streaming API.
//
// The response is split into:
//  1. One chunk per text character (simulating incremental text delivery)
//  2. One chunk per tool call (with complete arguments in one chunk for simplicity)
//  3. A final chunk with finish_reason and usage
//
// This is sufficient for testing the StreamAccumulator and StreamingToolExecutor
// without a real API connection.
func NewStreamReaderFromResponse(resp *types.ChatCompletionResponse) *llm.StreamReader {
	if resp == nil || len(resp.Choices) == 0 {
		return newStreamReaderFromSSE("data: [DONE]\n\n")
	}

	choice := resp.Choices[0]
	msg := choice.Message
	var lines []string

	// Emit text content as chunks (batch in groups of 20 chars for efficiency).
	text := msg.Content.String()
	for i := 0; i < len(text); i += 20 {
		end := i + 20
		if end > len(text) {
			end = len(text)
		}
		chunk := types.ChatCompletionChunk{
			Model: resp.Model,
			Choices: []types.StreamChoice{{
				Index: 0,
				Delta: types.Message{
					Content: types.NewTextContent(text[i:end]),
				},
			}},
		}
		data, _ := json.Marshal(chunk)
		lines = append(lines, "data: "+string(data))
	}

	// Emit tool calls: one chunk with id+name, then one with arguments.
	for i, tc := range msg.ToolCalls {
		// First chunk: id + name (no arguments yet).
		startChunk := types.ChatCompletionChunk{
			Model: resp.Model,
			Choices: []types.StreamChoice{{
				Index: 0,
				Delta: types.Message{
					ToolCalls: []types.ToolCall{{
						Index: i,
						ID:    tc.ID,
						Type:  "function",
						Function: types.ToolCallFunction{
							Name:      tc.Function.Name,
							Arguments: nil,
						},
					}},
				},
			}},
		}
		data, _ := json.Marshal(startChunk)
		lines = append(lines, "data: "+string(data))

		// Second chunk: arguments.
		argsChunk := types.ChatCompletionChunk{
			Model: resp.Model,
			Choices: []types.StreamChoice{{
				Index: 0,
				Delta: types.Message{
					ToolCalls: []types.ToolCall{{
						Index: i,
						Function: types.ToolCallFunction{
							Arguments: tc.Function.Arguments,
						},
					}},
				},
			}},
		}
		data, _ = json.Marshal(argsChunk)
		lines = append(lines, "data: "+string(data))
	}

	// Final chunk: finish_reason + usage.
	fr := choice.FinishReason
	finalChunk := types.ChatCompletionChunk{
		Model: resp.Model,
		Choices: []types.StreamChoice{{
			Index:        0,
			FinishReason: &fr,
		}},
		Usage: resp.Usage,
	}
	data, _ := json.Marshal(finalChunk)
	lines = append(lines, "data: "+string(data))
	lines = append(lines, "data: [DONE]")

	sseData := strings.Join(lines, "\n\n") + "\n\n"
	return newStreamReaderFromSSE(sseData)
}

// newStreamReaderFromSSE creates a StreamReader from raw SSE text.
func newStreamReaderFromSSE(sseData string) *llm.StreamReader {
	r := io.NopCloser(strings.NewReader(sseData))
	return llm.NewStreamReaderFromReadCloser(r)
}

// streamLLMClient wraps the base mock LLMClient with streaming support.
// It converts ChatCompletion responses into streaming chunks.
type streamLLMClient struct {
	base *LLMClient
}

// NewStreamingLLMClient wraps a mock LLMClient to also support streaming.
func NewStreamingLLMClient() *streamLLMClient {
	return &streamLLMClient{base: NewLLMClient()}
}

func (s *streamLLMClient) ChatCompletion(ctx context.Context, req types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	return s.base.ChatCompletion(ctx, req)
}

func (s *streamLLMClient) StreamChatCompletion(ctx context.Context, req types.ChatCompletionRequest) (*llm.StreamReader, error) {
	// Get the response that ChatCompletion would return.
	resp, err := s.base.ChatCompletion(ctx, req)
	if err != nil {
		return nil, err
	}
	return NewStreamReaderFromResponse(resp), nil
}

func (s *streamLLMClient) Usage() *llm.SessionUsage { return s.base.Usage() }

// SetResponses configures the response queue.
func (s *streamLLMClient) SetResponses(responses ...*types.ChatCompletionResponse) {
	s.base.Responses = responses
}

// SetErrors configures the error queue.
func (s *streamLLMClient) SetErrors(errs ...error) {
	s.base.Errors = errs
}

// CallCount returns the number of API calls made.
func (s *streamLLMClient) CallCount() int { return s.base.CallCount }

// Unexported helper to format SSE line for debugging.
func formatSSELine(chunk types.ChatCompletionChunk) string {
	data, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s", data)
}

// Ensure we don't need bufio externally.
var _ = bufio.Scanner{}
