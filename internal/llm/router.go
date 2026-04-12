// Model routing: per-pipeline and per-agent-role model selection with fallback chains.
//
// Business rationale: daily billions of ad reviews cannot all use the strongest
// (most expensive) model. Low-risk ads use cheap fast models; high-risk ads and
// adjudication use the strongest reasoning models. When a provider is overloaded
// (529), the system automatically degrades to a fallback model rather than stalling.
//
// The routing dimension is pipeline risk level × agent role, designed for batch
// ad review rather than interactive user sessions.
package llm

import (
	"fmt"
	"log/slog"
	"strings"
)

// RoutingConfig is the serializable model routing configuration.
// Loaded from config.json or overridden by environment variables.
type RoutingConfig struct {
	// Routes maps "pipeline" or "pipeline:role" to a model name.
	// More specific keys (with role) take priority over pipeline-only keys.
	Routes map[string]string `json:"routes"`

	// Fallbacks maps a model to its degraded alternative.
	// Chains are followed: grok-4.20-* → grok-4-1-fast-reasoning → gpt-4o.
	Fallbacks map[string]string `json:"fallbacks"`

	// Default is the model used when no route matches.
	Default string `json:"default"`
}

// DefaultRoutingConfig returns the xAI model tiering for ad review.
func DefaultRoutingConfig() RoutingConfig {
	return RoutingConfig{
		Routes: map[string]string{
			// Low risk: no reasoning needed, cheapest and fastest.
			"fast": "grok-4-1-fast-non-reasoning",
			// Medium risk: balanced cost and quality (current default model).
			"standard": "grok-4-1-fast-reasoning",
			// High risk: strongest reasoning model with verified tool calling support.
			// Note: grok-4.20-multi-agent-0309 uses a proprietary protocol incompatible
			// with OpenAI /v1/chat/completions, so we use grok-4.20-0309-reasoning instead.
			"comprehensive": "grok-4.20-0309-reasoning",
			// Coordinator always uses strongest reasoning regardless of pipeline.
			"comprehensive:coordinator": "grok-4.20-0309-reasoning",
			// Appeal re-review needs independent strong reasoning.
			"appeal": "grok-4.20-0309-reasoning",
		},
		Fallbacks: map[string]string{
			// grok-4.20 series → fall back to grok-4-1 series
			"grok-4.20-0309-reasoning": "grok-4-1-fast-reasoning",
			// grok-4-1 series → fall back to OpenAI (cross-provider)
			"grok-4-1-fast-reasoning":     "gpt-4o",
			"grok-4-1-fast-non-reasoning": "gpt-4o-mini",
		},
		Default: "grok-4-1-fast-reasoning",
	}
}

// ModelRouter selects the appropriate model for each review context.
type ModelRouter struct {
	routes    map[string]string
	fallbacks map[string]string
	dflt      string
	logger    *slog.Logger
}

// NewModelRouter creates a router from the given configuration.
func NewModelRouter(cfg RoutingConfig, logger *slog.Logger) *ModelRouter {
	r := &ModelRouter{
		routes:    make(map[string]string, len(cfg.Routes)),
		fallbacks: make(map[string]string, len(cfg.Fallbacks)),
		dflt:      cfg.Default,
		logger:    logger,
	}
	for k, v := range cfg.Routes {
		r.routes[k] = v
	}
	for k, v := range cfg.Fallbacks {
		r.fallbacks[k] = v
	}
	if r.dflt == "" {
		r.dflt = "grok-4-1-fast-reasoning"
	}
	return r
}

// RouteModel selects the model for a given pipeline and agent role.
//
// Lookup priority (2-level, more specific keys take precedence):
//  1. Exact "pipeline:role" key (e.g., "comprehensive:coordinator")
//  2. Pipeline-only key (e.g., "comprehensive")
//  3. Default model
func (r *ModelRouter) RouteModel(pipeline, role string) string {
	if role != "" {
		key := pipeline + ":" + role
		if model, ok := r.routes[key]; ok {
			return model
		}
	}
	if model, ok := r.routes[pipeline]; ok {
		return model
	}
	return r.dflt
}

// GetFallback returns the degraded model for the given model.
// Returns (fallback, true) if a fallback exists, or ("", false) if the model
// is already the lowest tier or has no configured fallback.
func (r *ModelRouter) GetFallback(model string) (string, bool) {
	fb, ok := r.fallbacks[model]
	if !ok || fb == model {
		return "", false
	}
	return fb, true
}

// FormatRoutingTable returns a human-readable summary for demo output.
func (r *ModelRouter) FormatRoutingTable() string {
	var b strings.Builder
	b.WriteString("=== Model Routing ===\n")
	for _, entry := range []struct{ key, label string }{
		{"fast", "fast"},
		{"standard", "standard"},
		{"comprehensive", "comprehensive"},
		{"comprehensive:coordinator", "coordinator"},
		{"appeal", "appeal"},
	} {
		model := r.RouteModel(entry.key, "")
		if strings.Contains(entry.key, ":") {
			parts := strings.SplitN(entry.key, ":", 2)
			model = r.RouteModel(parts[0], parts[1])
		}
		b.WriteString(fmt.Sprintf("  %-22s → %s\n", entry.label, model))
	}

	// Show fallback chain for the strongest model.
	b.WriteString("  fallback chain: ")
	chain := r.formatFallbackChain("grok-4.20-0309-reasoning")
	b.WriteString(chain)
	b.WriteString("\n")

	return b.String()
}

// formatFallbackChain builds "model1 → model2 → model3".
func (r *ModelRouter) formatFallbackChain(start string) string {
	var parts []string
	seen := make(map[string]bool)
	current := start
	for {
		if seen[current] {
			break // prevent infinite loops
		}
		parts = append(parts, current)
		seen[current] = true
		next, ok := r.fallbacks[current]
		if !ok || next == current {
			break
		}
		current = next
	}
	return strings.Join(parts, " → ")
}
