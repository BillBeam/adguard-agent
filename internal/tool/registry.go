package tool

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/store"
	"github.com/BillBeam/adguard-agent/internal/strategy"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// Registry manages the lifecycle and lookup of review tools.
// Thread-safe for concurrent reads (tool lookup during parallel execution).
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool to the registry. Panics on duplicate name.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name()]; exists {
		panic(fmt.Sprintf("duplicate tool registration: %s", t.Name()))
	}
	r.tools[t.Name()] = t
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// All returns all registered tools.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t)
	}
	return result
}

// ExportDefinitions converts all tools to OpenAI function calling format.
// This bridges to LoopConfig.Tools []types.ToolDefinition.
func (r *Registry) ExportDefinitions() []types.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]types.ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, ExportDefinition(t))
	}
	return defs
}

// Sub creates a sub-registry containing only the named tools.
// Used by the Multi-Agent orchestrator to give each specialist agent
// a restricted tool set. Unknown names are silently skipped.
func (r *Registry) Sub(names ...string) *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sub := NewRegistry()
	for _, name := range names {
		if t, ok := r.tools[name]; ok {
			sub.tools[name] = t
		}
	}
	return sub
}

// NewReviewRegistry creates a registry pre-loaded with all review tools.
// When rs is nil, HistoryLookup falls back to in-memory mode (backward compatible).
func NewReviewRegistry(
	client llm.LLMClient,
	matrix *strategy.StrategyMatrix,
	rs *store.ReviewStore,
	logger *slog.Logger,
) *Registry {
	reg := NewRegistry()
	reg.Register(NewContentAnalyzer(client, matrix, logger))
	reg.Register(NewPolicyMatcher(matrix, logger))
	reg.Register(NewRegionCompliance(matrix, logger))
	reg.Register(NewLandingPageChecker(client, logger))
	hl := NewHistoryLookup(logger)
	if rs != nil {
		hl.WithReviewStore(rs)
	}
	reg.Register(hl)
	reg.Register(NewPolicyKBLookup(matrix, logger))
	return reg
}
