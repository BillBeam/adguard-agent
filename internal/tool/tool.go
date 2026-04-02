// Package tool implements the Tool System for the AdGuard Agent — the interface,
// registry, executor, and 5 ad review business tools.
//
// Design pattern: fail-closed defaults. Every tool is assumed non-concurrent
// and non-readonly until explicitly declared otherwise. This prevents
// accidental parallel execution of tools with side effects.
package tool

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// Tool defines the interface for a review tool in the AdGuard Agent system.
//
// Five concern groups:
//
//	Identity:    Name, Description
//	Schema:      InputSchema (JSON Schema for OpenAI function calling)
//	Safety:      IsConcurrencySafe, IsReadOnly (fail-closed defaults: both false)
//	Validation:  ValidateInput
//	Execution:   Execute, MaxResultSize
//
// Phase 5 expansion points: PreToolHook, PostToolHook, CheckPermissions.
type Tool interface {
	// --- Identity ---

	// Name returns the tool's unique identifier used in function calling.
	Name() string
	// Description returns the tool's purpose description for the LLM.
	Description() string

	// --- Schema ---

	// InputSchema returns the JSON Schema for the tool's input parameters,
	// compatible with OpenAI function calling format.
	InputSchema() json.RawMessage

	// --- Safety (fail-closed defaults) ---

	// IsConcurrencySafe returns whether this tool can execute concurrently
	// with other concurrency-safe tools. Default: false.
	IsConcurrencySafe() bool
	// IsReadOnly returns whether this tool only reads data without side effects.
	// Default: false.
	IsReadOnly() bool

	// --- Validation ---

	// ValidateInput checks the raw JSON arguments before execution.
	// Returns nil if valid, a descriptive error otherwise.
	ValidateInput(args json.RawMessage) error

	// --- Execution ---

	// Execute runs the tool with the given arguments and returns the result
	// as a JSON string. Errors are propagated to the LLM as error messages,
	// never crashing the agentic loop.
	Execute(ctx context.Context, args json.RawMessage) (string, error)
	// MaxResultSize returns the maximum result size in bytes.
	// Results exceeding this are truncated. 0 means no limit.
	MaxResultSize() int
}

// BaseTool provides default implementations for safety and size fields.
// Embed this in concrete tool structs to inherit fail-closed defaults.
//
// Default values: IsConcurrencySafe=false, IsReadOnly=false, MaxResultSize=0.
// This is the fail-closed philosophy: unknown tools are assumed to be
// non-concurrent and non-readonly until explicitly overridden.
type BaseTool struct {
	concurrencySafe bool
	readOnly        bool
	maxResultSize   int
}

func (b BaseTool) IsConcurrencySafe() bool { return b.concurrencySafe }
func (b BaseTool) IsReadOnly() bool        { return b.readOnly }
func (b BaseTool) MaxResultSize() int      { return b.maxResultSize }

// ReviewToolBase returns a BaseTool configured for ad review tools:
// IsConcurrencySafe=true, IsReadOnly=true, MaxResultSize=32KB.
// All 5 ad review tools are read-only analysis tools safe for concurrent execution.
func ReviewToolBase() BaseTool {
	return BaseTool{
		concurrencySafe: true,
		readOnly:        true,
		maxResultSize:   32768,
	}
}

// isJSONString returns true if the raw JSON is a quoted string (not an object/array).
// LLMs sometimes pass a plain string instead of an object for tool arguments.
func isJSONString(data json.RawMessage) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && trimmed[0] == '"'
}

// unwrapJSONString extracts the Go string from a JSON-encoded string.
func unwrapJSONString(data json.RawMessage) string {
	var s string
	if json.Unmarshal(data, &s) == nil {
		return s
	}
	return string(data)
}

// ExportDefinition converts a Tool into a types.ToolDefinition for the
// OpenAI function calling wire format.
func ExportDefinition(t Tool) types.ToolDefinition {
	return types.ToolDefinition{
		Type: "function",
		Function: types.FunctionSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.InputSchema(),
		},
	}
}
