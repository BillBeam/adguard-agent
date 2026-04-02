package compact

import (
	"testing"

	"github.com/BillBeam/adguard-agent/internal/types"
)

func TestBudget_ReviewExhausted(t *testing.T) {
	tb := NewTokenBudget(BudgetConfig{
		MaxTokensPerReview:   1000,
		CompletionThreshold:  0.9,
		DiminishingThreshold: 500,
		DiminishingMinChecks: 3,
	})

	// Use 800 tokens (below 90% of 1000 = 900).
	tb.RecordUsage(types.Usage{PromptTokens: 400, CompletionTokens: 400})
	if reason := tb.Check(); reason != BudgetOK {
		t.Errorf("expected OK at 800/1000, got %s", reason)
	}

	// Use 200 more → total 1000 (>= 900 threshold).
	tb.RecordUsage(types.Usage{PromptTokens: 100, CompletionTokens: 100})
	if reason := tb.Check(); reason != BudgetReviewExhausted {
		t.Errorf("expected review exhausted at 1000/1000, got %s", reason)
	}
}

func TestBudget_BatchExhausted(t *testing.T) {
	tb := NewTokenBudget(BudgetConfig{
		MaxTokensPerBatch:   2000,
		CompletionThreshold: 0.9,
		DiminishingMinChecks: 10, // high to avoid diminishing trigger
	})

	// Review 1: 1000 tokens.
	tb.RecordUsage(types.Usage{PromptTokens: 500, CompletionTokens: 500})
	tb.ResetForReview()

	// Review 2: 900 more → batch total 1900 (>= 1800 threshold).
	tb.RecordUsage(types.Usage{PromptTokens: 450, CompletionTokens: 450})
	if reason := tb.Check(); reason != BudgetBatchExhausted {
		t.Errorf("expected batch exhausted at 1900/2000, got %s", reason)
	}
}

func TestBudget_DiminishingReturns(t *testing.T) {
	tb := NewTokenBudget(BudgetConfig{
		MaxTokensPerReview:   100000, // high, won't trigger
		CompletionThreshold:  0.9,
		DiminishingThreshold: 500,
		DiminishingMinChecks: 3,
	})

	// 3 calls with decreasing completion tokens.
	tb.RecordUsage(types.Usage{PromptTokens: 100, CompletionTokens: 1000})
	tb.RecordUsage(types.Usage{PromptTokens: 100, CompletionTokens: 300})
	tb.RecordUsage(types.Usage{PromptTokens: 100, CompletionTokens: 200})

	// checkCount=3, last two deltas (300, 200) both < 500.
	if reason := tb.Check(); reason != BudgetDiminishingReturns {
		t.Errorf("expected diminishing returns, got %s", reason)
	}
}

func TestBudget_DiminishingReturns_NotTriggeredEarly(t *testing.T) {
	tb := NewTokenBudget(BudgetConfig{
		MaxTokensPerReview:   100000,
		CompletionThreshold:  0.9,
		DiminishingThreshold: 500,
		DiminishingMinChecks: 3,
	})

	// Only 2 calls — below DiminishingMinChecks.
	tb.RecordUsage(types.Usage{PromptTokens: 100, CompletionTokens: 100})
	tb.RecordUsage(types.Usage{PromptTokens: 100, CompletionTokens: 100})

	if reason := tb.Check(); reason != BudgetOK {
		t.Errorf("expected OK with only 2 checks, got %s", reason)
	}
}

func TestBudget_DiminishingReturns_NotTriggeredWithHighDelta(t *testing.T) {
	tb := NewTokenBudget(BudgetConfig{
		MaxTokensPerReview:   100000,
		CompletionThreshold:  0.9,
		DiminishingThreshold: 500,
		DiminishingMinChecks: 3,
	})

	// 3 calls, but last delta is high.
	tb.RecordUsage(types.Usage{PromptTokens: 100, CompletionTokens: 200})
	tb.RecordUsage(types.Usage{PromptTokens: 100, CompletionTokens: 200})
	tb.RecordUsage(types.Usage{PromptTokens: 100, CompletionTokens: 800})

	if reason := tb.Check(); reason != BudgetOK {
		t.Errorf("expected OK when last delta is high, got %s", reason)
	}
}

func TestBudget_ResetForReview(t *testing.T) {
	tb := NewTokenBudget(BudgetConfig{
		MaxTokensPerReview: 1000,
		MaxTokensPerBatch:  10000,
		CompletionThreshold: 0.9,
		DiminishingMinChecks: 10,
	})

	// Use 500 tokens.
	tb.RecordUsage(types.Usage{PromptTokens: 250, CompletionTokens: 250})

	// Reset for new review.
	tb.ResetForReview()

	// Review tokens should be 0, batch should retain 500.
	if tb.reviewTokens != 0 {
		t.Errorf("expected review tokens reset to 0, got %d", tb.reviewTokens)
	}
	if tb.batchTokens != 500 {
		t.Errorf("expected batch tokens=500, got %d", tb.batchTokens)
	}
	if tb.checkCount != 0 {
		t.Errorf("expected check count reset, got %d", tb.checkCount)
	}
}

func TestBudget_DisabledLimits(t *testing.T) {
	tb := NewTokenBudget(BudgetConfig{
		MaxTokensPerReview:   0, // disabled
		MaxTokensPerBatch:    0, // disabled
		DiminishingMinChecks: 10,
	})

	tb.RecordUsage(types.Usage{PromptTokens: 50000, CompletionTokens: 50000})
	if reason := tb.Check(); reason != BudgetOK {
		t.Errorf("expected OK with disabled limits, got %s", reason)
	}
}

func TestBudget_Summary(t *testing.T) {
	tb := NewTokenBudget(DefaultBudgetConfig())
	tb.RecordUsage(types.Usage{PromptTokens: 100, CompletionTokens: 200})

	summary := tb.Summary()
	if summary == "" {
		t.Error("expected non-empty summary")
	}
}
