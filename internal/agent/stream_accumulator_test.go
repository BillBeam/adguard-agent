package agent

import (
	"context"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/types"
)

func TestAccumulator_TextOnly(t *testing.T) {
	acc := NewStreamAccumulator(nil, testLogger())

	// Simulate 3 text chunks.
	for _, text := range []string{"Hello", " world", "!"} {
		acc.ProcessChunk(&types.ChatCompletionChunk{
			Choices: []types.StreamChoice{{
				Index: 0,
				Delta: types.Message{Content: types.NewTextContent(text)},
			}},
		})
	}

	// Final chunk with finish_reason.
	fr := "stop"
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Index:        0,
			FinishReason: &fr,
		}},
	})
	acc.Finalize()

	resp := acc.BuildResponse()
	if resp.Choices[0].Message.Content.String() != "Hello world!" {
		t.Errorf("text = %q, want 'Hello world!'", resp.Choices[0].Message.Content.String())
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want 'stop'", resp.Choices[0].FinishReason)
	}
}

func TestAccumulator_SingleToolCall(t *testing.T) {
	acc := NewStreamAccumulator(nil, testLogger())

	// Tool call: first chunk has id + name.
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Index: 0,
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0,
					ID:    "call_001",
					Type:  "function",
					Function: types.ToolCallFunction{
						Name:      "analyze_content",
						Arguments: nil,
					},
				}},
			},
		}},
	})

	// Arguments come in fragments.
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Index: 0,
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0,
					Function: types.ToolCallFunction{
						Arguments: []byte(`{"head`),
					},
				}},
			},
		}},
	})
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Index: 0,
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0,
					Function: types.ToolCallFunction{
						Arguments: []byte(`line":"test"}`),
					},
				}},
			},
		}},
	})

	// Final chunk.
	fr := "tool_calls"
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Index:        0,
			FinishReason: &fr,
		}},
	})
	acc.Finalize()

	resp := acc.BuildResponse()
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.Choices[0].Message.ToolCalls))
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_001" {
		t.Errorf("ID = %q, want call_001", tc.ID)
	}
	if tc.Function.Name != "analyze_content" {
		t.Errorf("Name = %q, want analyze_content", tc.Function.Name)
	}
	if string(tc.Function.Arguments) != `{"headline":"test"}` {
		t.Errorf("Arguments = %s, want {\"headline\":\"test\"}", string(tc.Function.Arguments))
	}
}

func TestAccumulator_MultipleToolCalls(t *testing.T) {
	// Use a streaming executor to verify AddTool was called correctly.
	testExec := &trackingExecutor{}
	se := NewStreamingToolExecutor(context.Background(), testExec, nil, testLogger())
	acc := NewStreamAccumulator(se, testLogger())

	// First tool call (index 0).
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0, ID: "c1", Type: "function",
					Function: types.ToolCallFunction{Name: "tool_a", Arguments: []byte(`{"a":1}`)},
				}},
			},
		}},
	})

	// Second tool call starts (index 1) — should trigger submission of index 0.
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 1, ID: "c2", Type: "function",
					Function: types.ToolCallFunction{Name: "tool_b", Arguments: []byte(`{"b":2}`)},
				}},
			},
		}},
	})

	// After index 1 appears, only index 0 has been submitted (index 1 not yet complete).
	if se.ToolCount() != 1 {
		t.Errorf("after index 1 appears, ToolCount = %d, want 1 (only index 0)", se.ToolCount())
	}

	// Finalize submits index 1.
	acc.Finalize()
	if se.ToolCount() != 2 {
		t.Errorf("after Finalize, ToolCount = %d, want 2", se.ToolCount())
	}

	// Verify the response has both tool calls.
	resp := acc.BuildResponse()
	if len(resp.Choices[0].Message.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls in response, got %d", len(resp.Choices[0].Message.ToolCalls))
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Name != "tool_a" {
		t.Errorf("tool[0] = %q, want tool_a", resp.Choices[0].Message.ToolCalls[0].Function.Name)
	}
	if resp.Choices[0].Message.ToolCalls[1].Function.Name != "tool_b" {
		t.Errorf("tool[1] = %q, want tool_b", resp.Choices[0].Message.ToolCalls[1].Function.Name)
	}
}

func TestAccumulator_FinishReasonSubmitsLast(t *testing.T) {
	testExec := &trackingExecutor{}
	se := NewStreamingToolExecutor(context.Background(), testExec, nil, testLogger())
	acc := NewStreamAccumulator(se, testLogger())

	// Single tool call.
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0, ID: "c1", Type: "function",
					Function: types.ToolCallFunction{Name: "my_tool", Arguments: []byte(`{}`)},
				}},
			},
		}},
	})

	// Before Finalize: tool should not yet be in executor (only one index, no new index to trigger).
	if se.ToolCount() != 0 {
		t.Errorf("before Finalize, ToolCount = %d, want 0", se.ToolCount())
	}

	// Finalize triggers submission of the last tool.
	acc.Finalize()
	if se.ToolCount() != 1 {
		t.Errorf("after Finalize, ToolCount = %d, want 1", se.ToolCount())
	}
}

func TestAccumulator_BuildResponse_Model(t *testing.T) {
	acc := NewStreamAccumulator(nil, testLogger())
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Model: "grok-4-1-fast-reasoning",
		Choices: []types.StreamChoice{{
			Delta: types.Message{Content: types.NewTextContent("hi")},
		}},
	})
	fr := "stop"
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{FinishReason: &fr}},
	})
	acc.Finalize()

	resp := acc.BuildResponse()
	if resp.Model != "grok-4-1-fast-reasoning" {
		t.Errorf("model = %q, want grok-4-1-fast-reasoning", resp.Model)
	}
}

// trackingExecutor is a minimal executor for accumulator tests.
type trackingExecutor struct{}

func (te *trackingExecutor) Execute(_ context.Context, toolCalls []types.ToolCall) ([]types.Message, error) {
	results := make([]types.Message, len(toolCalls))
	for i, tc := range toolCalls {
		results[i] = types.Message{
			Role:       types.RoleTool,
			Content:    types.NewTextContent(`{"result":"ok"}`),
			ToolCallID: tc.ID,
		}
	}
	return results, nil
}
