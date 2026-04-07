// Package types defines core data types shared across the AdGuard Agent system.
//
// The message types in this file implement the OpenAI Chat Completions API wire format.
// Design pattern: tagged-union message/content types with polymorphic JSON serialization.
// The Content type supports both string and []ContentBlock forms via custom marshal/unmarshal,
// matching the API's "content": "string" | "content": [{type, ...}] polymorphism.
package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// --- Role ---

// Role identifies the sender of a message in a conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// --- Content Block (tagged-union pattern) ---

// ContentBlock is a polymorphic content element within a message.
// Concrete types: TextBlock, ImageURLBlock.
// The type field serves as the discriminator for JSON deserialization.
type ContentBlock interface {
	contentBlockType() string
}

// TextBlock represents a text content element.
type TextBlock struct {
	Type string `json:"type"` // always "text"
	Text string `json:"text"`
}

func (TextBlock) contentBlockType() string { return "text" }

// ImageURLBlock represents an image reference content element.
type ImageURLBlock struct {
	Type     string   `json:"type"`      // always "image_url"
	ImageURL ImageURL `json:"image_url"`
}

func (ImageURLBlock) contentBlockType() string { return "image_url" }

// ImageURL holds the URL and detail level for an image content block.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "auto", "low", "high"
}

// --- Content (polymorphic: string | []ContentBlock) ---

// Content represents the polymorphic content field in OpenAI messages.
// When Text is set and Parts is nil, it serializes as a JSON string.
// When Parts is set, it serializes as a JSON array of content blocks.
type Content struct {
	Text  string
	Parts []ContentBlock
}

// NewTextContent creates a Content from a plain string.
func NewTextContent(text string) Content {
	return Content{Text: text}
}

// NewPartsContent creates a Content from multiple content blocks.
func NewPartsContent(parts ...ContentBlock) Content {
	return Content{Parts: parts}
}

// MarshalJSON implements the OpenAI wire format:
// - Parts non-nil → JSON array of typed objects
// - Otherwise → JSON string
func (c Content) MarshalJSON() ([]byte, error) {
	if c.Parts != nil {
		return json.Marshal(c.Parts)
	}
	return json.Marshal(c.Text)
}

// UnmarshalJSON handles both string and array forms.
// Tries string first; on failure, parses as []json.RawMessage and dispatches
// each element by its "type" discriminator field.
func (c *Content) UnmarshalJSON(data []byte) error {
	// Try string form first (most common for simple messages).
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.Text = s
		c.Parts = nil
		return nil
	}

	// Parse as array of raw messages.
	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(data, &rawBlocks); err != nil {
		return fmt.Errorf("content: expected string or array, got: %s", string(data[:min(len(data), 50)]))
	}

	// Peek at the "type" field to dispatch to concrete types.
	type typeProbe struct {
		Type string `json:"type"`
	}

	c.Parts = make([]ContentBlock, 0, len(rawBlocks))
	for i, raw := range rawBlocks {
		var probe typeProbe
		if err := json.Unmarshal(raw, &probe); err != nil {
			return fmt.Errorf("content block %d: missing type field: %w", i, err)
		}

		switch probe.Type {
		case "text":
			var block TextBlock
			if err := json.Unmarshal(raw, &block); err != nil {
				return fmt.Errorf("content block %d (text): %w", i, err)
			}
			c.Parts = append(c.Parts, block)
		case "image_url":
			var block ImageURLBlock
			if err := json.Unmarshal(raw, &block); err != nil {
				return fmt.Errorf("content block %d (image_url): %w", i, err)
			}
			c.Parts = append(c.Parts, block)
		default:
			return fmt.Errorf("content block %d: unknown type %q", i, probe.Type)
		}
	}
	c.Text = ""
	return nil
}

// String returns the text content. For multi-part content, concatenates text blocks.
func (c Content) String() string {
	if c.Parts == nil {
		return c.Text
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if tb, ok := p.(TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// --- Message ---

// Message is the wire-format message for the OpenAI Chat Completions API.
type Message struct {
	Role       Role       `json:"role"`
	Content    Content    `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// --- Tool Calling ---

// ToolCall represents a function call requested by the assistant.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction holds the name and raw JSON arguments of a tool call.
type ToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolDefinition defines a tool available to the model.
type ToolDefinition struct {
	Type     string       `json:"type"` // always "function"
	Function FunctionSpec `json:"function"`
}

// FunctionSpec describes a callable function's interface.
type FunctionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
	Strict      bool            `json:"strict,omitempty"`
}

// --- Request / Response ---

// ResponseFormat specifies the desired output structure.
type ResponseFormat struct {
	Type       string          `json:"type"`                  // "text" or "json_schema"
	JSONSchema json.RawMessage `json:"json_schema,omitempty"` // schema when type="json_schema"
}

// ChatCompletionRequest is the request body for /v1/chat/completions.
type ChatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Tools          []ToolDefinition `json:"tools,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	Stop           []string        `json:"stop,omitempty"`

	// Retry hints — not serialized to API, consumed by the LLM client's retry logic.
	// When FallbackModel is set, consecutive 529 errors trigger automatic model downgrade.
	RetryCurrentModel  string `json:"-"`
	RetryFallbackModel string `json:"-"`
}

// ChatCompletionResponse is the response from /v1/chat/completions (non-streaming).
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // "chat.completion"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice represents one completion option in the response.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"` // "stop", "tool_calls", "length"
}

// Usage reports token consumption for a single API call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// --- Streaming ---

// ChatCompletionChunk is a single SSE event in a streaming response.
type ChatCompletionChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"` // "chat.completion.chunk"
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"` // present in final chunk if requested
}

// StreamChoice represents a single delta in a streaming response.
type StreamChoice struct {
	Index        int     `json:"index"`
	Delta        Message `json:"delta"`
	FinishReason *string `json:"finish_reason"` // nil until stream ends
}

// --- Internal Envelope ---

// MessageEnvelope wraps an API Message with internal tracking metadata.
// Separates API wire concerns from system-internal state, following the pattern
// of wrapping API payloads with UUID, timestamp, and parent references.
type MessageEnvelope struct {
	Message         Message   `json:"message"`
	UUID            string    `json:"uuid"`
	Timestamp       time.Time `json:"timestamp"`
	ParentToolUseID string    `json:"parent_tool_use_id,omitempty"`
	RequestID       string    `json:"request_id,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
}
