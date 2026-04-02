package compact

import (
	"fmt"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// Token budget tracks and enforces token usage limits with diminishing
// returns detection to stop LLM "stalling" (producing minimal useful output).

// BudgetConfig controls TokenBudget behavior.
type BudgetConfig struct {
	// MaxTokensPerReview is the token limit for a single ad review.
	// Default: 50000. Set to 0 to disable.
	MaxTokensPerReview int

	// MaxTokensPerBatch is the total token limit for a batch of reviews.
	// Default: 500000. Set to 0 to disable.
	MaxTokensPerBatch int

	// CompletionThreshold is the usage ratio at which to stop.
	// Default: 0.9 (90%). Leaves 10% headroom for wrap-up before hard cutoff.
	CompletionThreshold float64

	// DiminishingThreshold is the minimum completion_tokens delta
	// to consider "productive". Two consecutive deltas below this
	// trigger early stop.
	// Default: 500.
	DiminishingThreshold int

	// DiminishingMinChecks is the minimum number of API calls before
	// diminishing returns detection activates.
	// Default: 3.
	DiminishingMinChecks int
}

// DefaultBudgetConfig returns production defaults.
func DefaultBudgetConfig() BudgetConfig {
	return BudgetConfig{
		MaxTokensPerReview:   50000,
		MaxTokensPerBatch:    500000,
		CompletionThreshold:  0.9,
		DiminishingThreshold: 500,
		DiminishingMinChecks: 3,
	}
}

// BudgetExhaustedReason describes why the budget check failed.
type BudgetExhaustedReason string

const (
	BudgetOK                 BudgetExhaustedReason = ""
	BudgetReviewExhausted    BudgetExhaustedReason = "review_budget_exhausted"
	BudgetBatchExhausted     BudgetExhaustedReason = "batch_budget_exhausted"
	BudgetDiminishingReturns BudgetExhaustedReason = "diminishing_returns"
)

// TokenBudget tracks and enforces token usage limits.
type TokenBudget struct {
	config BudgetConfig

	// Per-review counters (reset between reviews).
	reviewTokens int

	// Per-batch counters (accumulate across reviews).
	batchTokens int

	// Diminishing returns detection.
	recentDeltas []int
	checkCount   int
}

// NewTokenBudget creates a budget tracker.
func NewTokenBudget(config BudgetConfig) *TokenBudget {
	return &TokenBudget{
		config:       config,
		recentDeltas: make([]int, 0, 8),
	}
}

// RecordUsage records token consumption from one API call.
// Called by loop.go after ChatCompletion returns.
func (tb *TokenBudget) RecordUsage(usage types.Usage) {
	total := usage.PromptTokens + usage.CompletionTokens
	tb.reviewTokens += total
	tb.batchTokens += total

	// Track completion token deltas for diminishing returns detection.
	tb.recentDeltas = append(tb.recentDeltas, usage.CompletionTokens)
	tb.checkCount++
}

// Check evaluates budget constraints and returns the reason if exhausted.
// Returns BudgetOK ("") if within budget.
func (tb *TokenBudget) Check() BudgetExhaustedReason {
	// 1. Per-review budget check.
	if tb.config.MaxTokensPerReview > 0 {
		threshold := int(float64(tb.config.MaxTokensPerReview) * tb.config.CompletionThreshold)
		if tb.reviewTokens >= threshold {
			return BudgetReviewExhausted
		}
	}

	// 2. Per-batch budget check.
	if tb.config.MaxTokensPerBatch > 0 {
		threshold := int(float64(tb.config.MaxTokensPerBatch) * tb.config.CompletionThreshold)
		if tb.batchTokens >= threshold {
			return BudgetBatchExhausted
		}
	}

	// 3. Diminishing returns detection.
	if tb.checkCount >= tb.config.DiminishingMinChecks && len(tb.recentDeltas) >= 2 {
		last := tb.recentDeltas[len(tb.recentDeltas)-1]
		prev := tb.recentDeltas[len(tb.recentDeltas)-2]
		if last < tb.config.DiminishingThreshold && prev < tb.config.DiminishingThreshold {
			return BudgetDiminishingReturns
		}
	}

	return BudgetOK
}

// ResetForReview resets per-review counters for a new ad review.
// Batch counters continue accumulating.
func (tb *TokenBudget) ResetForReview() {
	tb.reviewTokens = 0
	tb.recentDeltas = tb.recentDeltas[:0]
	tb.checkCount = 0
}

// Summary returns a human-readable budget summary for logging.
func (tb *TokenBudget) Summary() string {
	return fmt.Sprintf("review=%d/%d batch=%d/%d checks=%d",
		tb.reviewTokens, tb.config.MaxTokensPerReview,
		tb.batchTokens, tb.config.MaxTokensPerBatch,
		tb.checkCount,
	)
}
