package agent

import (
	"context"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// ToolExecutor executes tool calls and returns tool_result messages.
// Phase 1 uses mock implementations; Phase 2 replaces with real tools.
type ToolExecutor interface {
	Execute(ctx context.Context, toolCalls []types.ToolCall) ([]types.Message, error)
}

// ToolRegistry manages tool definitions for the agentic loop.
// Phase 2 will expand this with dynamic loading and read/write partitioning.
type ToolRegistry struct {
	tools map[string]types.ToolDefinition
}

// NewToolRegistry creates an empty tool registry.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]types.ToolDefinition)}
}

// Register adds a tool definition to the registry.
func (r *ToolRegistry) Register(tool types.ToolDefinition) {
	r.tools[tool.Function.Name] = tool
}

// Get returns a tool definition by name.
func (r *ToolRegistry) Get(name string) (types.ToolDefinition, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// All returns all registered tool definitions.
func (r *ToolRegistry) All() []types.ToolDefinition {
	result := make([]types.ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}
