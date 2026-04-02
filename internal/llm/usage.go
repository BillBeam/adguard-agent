package llm

import (
	"fmt"
	"strings"
	"sync"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// ModelCosts defines the per-million-token pricing for a model.
type ModelCosts struct {
	InputPerMtok  float64
	OutputPerMtok float64
}

// modelPricing maps model names to their token pricing.
// Extensible: add new models as providers are integrated.
var modelPricing = map[string]ModelCosts{
	"grok-4-1-fast-reasoning": {InputPerMtok: 2.00, OutputPerMtok: 10.00},
	"grok-3-mini-beta":        {InputPerMtok: 0.30, OutputPerMtok: 0.50},
	"grok-3-beta":             {InputPerMtok: 3.00, OutputPerMtok: 15.00},
	"grok-3-mini-fast":        {InputPerMtok: 0.60, OutputPerMtok: 1.00},
	"gpt-4o":                  {InputPerMtok: 2.50, OutputPerMtok: 10.00},
	"gpt-4o-mini":             {InputPerMtok: 0.15, OutputPerMtok: 0.60},
}

// defaultCosts is used when a model's pricing is not in the table.
var defaultCosts = ModelCosts{InputPerMtok: 1.00, OutputPerMtok: 3.00}

// ModelUsage accumulates token counts and cost for a single model.
type ModelUsage struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// SessionUsage tracks token usage and cost across all API calls in a session.
// Thread-safe: all methods are guarded by a mutex, since the agentic loop may
// invoke multiple concurrent tool calls that report usage simultaneously.
type SessionUsage struct {
	mu        sync.Mutex
	models    map[string]*ModelUsage
	totalCost float64
}

// NewSessionUsage creates a new session usage tracker.
func NewSessionUsage() *SessionUsage {
	return &SessionUsage{
		models: make(map[string]*ModelUsage),
	}
}

// Add records token usage from a single API call.
// Calculates cost from the pricing table and accumulates per-model.
func (s *SessionUsage) Add(model string, usage types.Usage) {
	cost := calculateCost(model, usage)

	s.mu.Lock()
	defer s.mu.Unlock()

	mu, ok := s.models[model]
	if !ok {
		mu = &ModelUsage{}
		s.models[model] = mu
	}

	mu.InputTokens += usage.PromptTokens
	mu.OutputTokens += usage.CompletionTokens
	mu.CostUSD += cost
	s.totalCost += cost
}

// TotalCost returns the accumulated cost across all models.
func (s *SessionUsage) TotalCost() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalCost
}

// TotalTokens returns the total input and output tokens across all models.
func (s *SessionUsage) TotalTokens() (input, output int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, mu := range s.models {
		input += mu.InputTokens
		output += mu.OutputTokens
	}
	return
}

// ByModel returns a snapshot of per-model usage. The returned map is a copy.
func (s *SessionUsage) ByModel() map[string]ModelUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]ModelUsage, len(s.models))
	for k, v := range s.models {
		result[k] = *v
	}
	return result
}

// FormatReport returns a human-readable summary of session usage.
func (s *SessionUsage) FormatReport() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.models) == 0 {
		return "No API usage recorded"
	}

	var b strings.Builder
	b.WriteString("Session Usage:\n")
	for model, mu := range s.models {
		fmt.Fprintf(&b, "  %s: %d input + %d output tokens ($%.4f)\n",
			model, mu.InputTokens, mu.OutputTokens, mu.CostUSD)
	}
	fmt.Fprintf(&b, "  Total: $%.4f", s.totalCost)
	return b.String()
}

// calculateCost computes the USD cost of a single API call.
// Formula: (tokens / 1,000,000) * price_per_million_tokens
func calculateCost(model string, usage types.Usage) float64 {
	costs, ok := modelPricing[model]
	if !ok {
		costs = defaultCosts
	}

	inputCost := float64(usage.PromptTokens) / 1_000_000 * costs.InputPerMtok
	outputCost := float64(usage.CompletionTokens) / 1_000_000 * costs.OutputPerMtok
	return inputCost + outputCost
}
