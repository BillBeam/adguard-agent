// Package compact implements Context Management for the AdGuard Agent system.
//
// Core capabilities:
//   - Multi-layer cascading compression: MicroCompact → AutoCompact → ReactiveCompact
//   - LLM-driven summarization with structured prompt (6 dimensions for ad review)
//   - Circuit breaker for compression failures
//   - Token budget with diminishing returns detection
//
// The package name is "compact" (not "context") to avoid import conflicts
// with Go's built-in context package.
package compact

import "github.com/BillBeam/adguard-agent/internal/types"

// Token estimation constants.
//
// English text averages ~4 chars/token. We use chars/3 as a conservative
// factor — it's better to trigger compression early than to hit
// prompt_too_long errors.
const (
	// charsPerToken is the conservative estimate of characters per token.
	// English text averages ~4 chars/token; we use 3 for safety margin.
	charsPerToken = 3

	// messageOverhead accounts for role markers, separators, and JSON framing
	// per message in the API request.
	messageOverhead = 4
)

// EstimateTokens returns a conservative token count estimate for text.
// Uses len(text)/3 — intentionally over-estimates to ensure compression
// triggers before context window limits are hit.
func EstimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	return len(text)/charsPerToken + 1
}

// EstimateMessagesTokens estimates the total token count for a message sequence.
// Iterates over messages, summing Content.String() token estimates plus
// per-message overhead (role marker, separators).
func EstimateMessagesTokens(messages []types.Message) int {
	total := 0
	for _, msg := range messages {
		total += EstimateTokens(msg.Content.String()) + messageOverhead
	}
	return total
}
