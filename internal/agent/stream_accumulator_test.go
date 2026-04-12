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
	se := NewStreamingToolExecutor(context.Background(), testExec, nil, nil, nil, testLogger())
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

	// 两个工具调用的 JSON 都在各自 chunk 中完整——边界检测会立即提交两者。
	// index 0 因 "new index" 触发提交，index 1 因 JSON 边界检测立即提交。
	if se.ToolCount() != 2 {
		t.Errorf("after index 1 appears, ToolCount = %d, want 2 (both submitted by boundary detection)", se.ToolCount())
	}

	// Finalize 是幂等的——已提交的不会重复。
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
	se := NewStreamingToolExecutor(context.Background(), testExec, nil, nil, nil, testLogger())
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

	// JSON 边界检测：{} 在 chunk 中即完整，立即提交，不等 Finalize。
	if se.ToolCount() != 1 {
		t.Errorf("before Finalize, ToolCount = %d, want 1 (JSON boundary detected)", se.ToolCount())
	}

	// Finalize 幂等——已提交的不重复。
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

// --- JSON 边界检测测试 ---

func TestAccumulator_JSONBoundary_MultiChunkEarlySubmission(t *testing.T) {
	testExec := &trackingExecutor{}
	se := NewStreamingToolExecutor(context.Background(), testExec, nil, nil, nil, testLogger())
	acc := NewStreamAccumulator(se, testLogger())

	// 第一个 chunk：工具名 + 部分参数。
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0, ID: "c1", Type: "function",
					Function: types.ToolCallFunction{Name: "my_tool", Arguments: []byte(`{"head`)},
				}},
			},
		}},
	})
	if se.ToolCount() != 0 {
		t.Errorf("chunk 1: ToolCount = %d, want 0 (JSON incomplete)", se.ToolCount())
	}

	// 第二个 chunk：更多参数。
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0,
					Function: types.ToolCallFunction{Arguments: []byte(`line":"te`)},
				}},
			},
		}},
	})
	if se.ToolCount() != 0 {
		t.Errorf("chunk 2: ToolCount = %d, want 0 (JSON still incomplete)", se.ToolCount())
	}

	// 第三个 chunk：闭合大括号，JSON 完整。
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0,
					Function: types.ToolCallFunction{Arguments: []byte(`st"}`)},
				}},
			},
		}},
	})
	if se.ToolCount() != 1 {
		t.Errorf("chunk 3: ToolCount = %d, want 1 (JSON complete, should submit)", se.ToolCount())
	}
}

func TestAccumulator_JSONBoundary_NestedObjects(t *testing.T) {
	testExec := &trackingExecutor{}
	se := NewStreamingToolExecutor(context.Background(), testExec, nil, nil, nil, testLogger())
	acc := NewStreamAccumulator(se, testLogger())

	// 嵌套对象：{"outer":{"inner":1}} — 内层 } 不应触发提交。
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0, ID: "c1", Type: "function",
					Function: types.ToolCallFunction{Name: "t", Arguments: []byte(`{"outer":{"inner":1}`)},
				}},
			},
		}},
	})
	if se.ToolCount() != 0 {
		t.Errorf("nested inner close: ToolCount = %d, want 0", se.ToolCount())
	}

	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0,
					Function: types.ToolCallFunction{Arguments: []byte(`}`)},
				}},
			},
		}},
	})
	if se.ToolCount() != 1 {
		t.Errorf("outer close: ToolCount = %d, want 1", se.ToolCount())
	}
}

func TestAccumulator_JSONBoundary_StringWithBraces(t *testing.T) {
	testExec := &trackingExecutor{}
	se := NewStreamingToolExecutor(context.Background(), testExec, nil, nil, nil, testLogger())
	acc := NewStreamAccumulator(se, testLogger())

	// 字符串内的大括号不应影响深度追踪。
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0, ID: "c1", Type: "function",
					Function: types.ToolCallFunction{Name: "t", Arguments: []byte(`{"text":"a } b { c"}`)},
				}},
			},
		}},
	})
	if se.ToolCount() != 1 {
		t.Errorf("string with braces: ToolCount = %d, want 1", se.ToolCount())
	}
}

func TestAccumulator_JSONBoundary_EscapedQuotes(t *testing.T) {
	testExec := &trackingExecutor{}
	se := NewStreamingToolExecutor(context.Background(), testExec, nil, nil, nil, testLogger())
	acc := NewStreamAccumulator(se, testLogger())

	// 转义引号 \" 不应退出字符串上下文。
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0, ID: "c1", Type: "function",
					Function: types.ToolCallFunction{Name: "t", Arguments: []byte(`{"val":"a\"b}c"}`)},
				}},
			},
		}},
	})
	if se.ToolCount() != 1 {
		t.Errorf("escaped quotes: ToolCount = %d, want 1", se.ToolCount())
	}
}

func TestAccumulator_JSONBoundary_ArrayInObject(t *testing.T) {
	testExec := &trackingExecutor{}
	se := NewStreamingToolExecutor(context.Background(), testExec, nil, nil, nil, testLogger())
	acc := NewStreamAccumulator(se, testLogger())

	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0, ID: "c1", Type: "function",
					Function: types.ToolCallFunction{Name: "t", Arguments: []byte(`{"items":[1,2,3]}`)},
				}},
			},
		}},
	})
	if se.ToolCount() != 1 {
		t.Errorf("array in object: ToolCount = %d, want 1", se.ToolCount())
	}
}

func TestAccumulator_JSONBoundary_IncompleteStillNeedsFinalize(t *testing.T) {
	testExec := &trackingExecutor{}
	se := NewStreamingToolExecutor(context.Background(), testExec, nil, nil, nil, testLogger())
	acc := NewStreamAccumulator(se, testLogger())

	// JSON 不完整（缺闭合括号），只能靠 Finalize 提交。
	acc.ProcessChunk(&types.ChatCompletionChunk{
		Choices: []types.StreamChoice{{
			Delta: types.Message{
				ToolCalls: []types.ToolCall{{
					Index: 0, ID: "c1", Type: "function",
					Function: types.ToolCallFunction{Name: "t", Arguments: []byte(`{"incomplete":true`)},
				}},
			},
		}},
	})
	if se.ToolCount() != 0 {
		t.Errorf("incomplete JSON: ToolCount = %d, want 0", se.ToolCount())
	}

	acc.Finalize()
	if se.ToolCount() != 1 {
		t.Errorf("after Finalize: ToolCount = %d, want 1", se.ToolCount())
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
