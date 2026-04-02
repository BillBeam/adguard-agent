// Package mock provides test doubles for the agentic loop.
package mock

import (
	"context"
	"fmt"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// LLMClient is a programmable mock LLM client for testing the agentic loop.
// Responses are returned in sequence; when exhausted, a default MANUAL_REVIEW is returned.
type LLMClient struct {
	Responses []*types.ChatCompletionResponse
	Errors    []error
	CallCount int
	usage     *llm.SessionUsage
}

// NewLLMClient creates a new mock LLM client.
func NewLLMClient() *LLMClient {
	return &LLMClient{usage: llm.NewSessionUsage()}
}

func (m *LLMClient) ChatCompletion(_ context.Context, _ types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	idx := m.CallCount
	m.CallCount++

	if idx < len(m.Errors) && m.Errors[idx] != nil {
		return nil, m.Errors[idx]
	}

	if idx < len(m.Responses) {
		return m.Responses[idx], nil
	}

	// Default: return stop with MANUAL_REVIEW.
	return &types.ChatCompletionResponse{
		Choices: []types.Choice{{
			Message:      types.Message{Role: types.RoleAssistant, Content: types.NewTextContent(`{"decision":"MANUAL_REVIEW","confidence":0.0,"violations":[],"reasoning":"default mock response"}`)},
			FinishReason: "stop",
		}},
	}, nil
}

func (m *LLMClient) StreamChatCompletion(_ context.Context, _ types.ChatCompletionRequest) (*llm.StreamReader, error) {
	return nil, fmt.Errorf("streaming not supported in mock")
}

func (m *LLMClient) Usage() *llm.SessionUsage {
	return m.usage
}
