// Package llm implements the LLM client with multi-provider support, retry logic,
// and usage tracking. Key patterns: exponential backoff with jitter, per-error-type
// retry classification, and client-side request tracing.
package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"strconv"
	"time"
)

const (
	// Retry backoff constants — base delay doubles per attempt, capped at maxDelay.
	baseDelayMS = 500
	maxDelayMS  = 32000

	// Default maximum retry attempts before giving up.
	defaultMaxRetries = 10

	// Max529Retries is the number of consecutive 529 (overloaded) errors before
	// triggering a model fallback to a cheaper/different-provider model.
	Max529Retries = 3
)

// APIError represents an HTTP API error with status code.
// Wraps the underlying error while carrying the status code for retry classification.
type APIError struct {
	StatusCode int
	Message    string
	RetryAfter string // from Retry-After header, if present
	Err        error
}

func (e *APIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("api error %d: %s: %v", e.StatusCode, e.Message, e.Err)
	}
	return fmt.Sprintf("api error %d: %s", e.StatusCode, e.Message)
}

func (e *APIError) Unwrap() error { return e.Err }

// FallbackTriggeredError indicates that consecutive 529 errors have triggered
// a model downgrade. The caller should retry the request with FallbackModel.
//
// The caller (ReviewEngine) catches this error and retries with FallbackModel.
type FallbackTriggeredError struct {
	OriginalModel string
	FallbackModel string
	Consecutive   int
}

func (e *FallbackTriggeredError) Error() string {
	return fmt.Sprintf("model fallback triggered: %d consecutive 529 errors, %s → %s",
		e.Consecutive, e.OriginalModel, e.FallbackModel)
}

// RetryOptions configures the retry behavior.
type RetryOptions struct {
	MaxRetries    int
	CurrentModel  string // for 529 fallback error message
	FallbackModel string // "" = no fallback on 529
}

// withRetry executes an operation with exponential backoff retry.
//
// The retry strategy classifies errors by HTTP status code:
//   - 429 (rate limit): retry with Retry-After header if present
//   - 529 (overloaded): retry up to Max529Retries, then trigger FallbackTriggeredError
//   - 5xx (server error): retry
//   - Connection errors: retry (ECONNRESET, EPIPE, timeout)
//   - 4xx (client error, except 429): do NOT retry
//   - Context cancelled/deadline: do NOT retry
//
// Backoff formula: min(baseDelay * 2^(attempt-1), maxDelay) + jitter
// Jitter: random 0-25% of base delay to prevent thundering herd.
func withRetry[T any](
	ctx context.Context,
	logger *slog.Logger,
	opts RetryOptions,
	operation func(ctx context.Context, attempt int) (T, error),
) (T, error) {
	maxRetries := opts.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}

	var (
		lastErr        error
		consecutive529 int
	)
	for attempt := 1; attempt <= maxRetries+1; attempt++ {
		result, err := operation(ctx, attempt)
		if err == nil {
			return result, nil
		}
		lastErr = err

		// Track consecutive 529s for model fallback.
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 529 {
			consecutive529++
			if consecutive529 >= Max529Retries && opts.FallbackModel != "" {
				var zero T
				return zero, &FallbackTriggeredError{
					OriginalModel: opts.CurrentModel,
					FallbackModel: opts.FallbackModel,
					Consecutive:   consecutive529,
				}
			}
		} else {
			consecutive529 = 0 // non-529 resets the counter
		}

		// Check if we should retry this error.
		if !shouldRetry(err) {
			var zero T
			return zero, err
		}

		// Check context before sleeping.
		if ctx.Err() != nil {
			var zero T
			return zero, fmt.Errorf("context cancelled during retry: %w", ctx.Err())
		}

		// Don't sleep after the last attempt.
		if attempt > maxRetries {
			break
		}

		delay := retryDelay(attempt, retryAfterFromError(err))
		logger.Warn("retrying API request",
			slog.Int("attempt", attempt),
			slog.Duration("delay", delay),
			slog.String("error", err.Error()),
			slog.Int("consecutive_529", consecutive529),
		)

		select {
		case <-ctx.Done():
			var zero T
			return zero, fmt.Errorf("context cancelled during retry wait: %w", ctx.Err())
		case <-time.After(delay):
			// Continue to next attempt.
		}
	}

	var zero T
	return zero, fmt.Errorf("max retries (%d) exceeded: %w", maxRetries, lastErr)
}

// retryDelay calculates the delay before the next retry attempt.
// Uses exponential backoff with jitter, respecting Retry-After if provided.
func retryDelay(attempt int, retryAfterSec string) time.Duration {
	// Honor Retry-After header if present (parsed as integer seconds).
	if retryAfterSec != "" {
		if seconds, err := strconv.Atoi(retryAfterSec); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}

	// Exponential backoff: base * 2^(attempt-1), capped at maxDelay.
	base := float64(baseDelayMS) * math.Pow(2, float64(attempt-1))
	if base > float64(maxDelayMS) {
		base = float64(maxDelayMS)
	}

	// Jitter: add 0-25% of the base delay to prevent thundering herd.
	jitter := rand.Float64() * 0.25 * base

	return time.Duration(base+jitter) * time.Millisecond
}

// shouldRetry determines if an error is retryable based on its type and status code.
func shouldRetry(err error) bool {
	// Never retry context cancellation.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Check for API errors with status codes.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == 429: // rate limited
			return true
		case apiErr.StatusCode == 529: // overloaded
			return true
		case apiErr.StatusCode >= 500: // server errors
			return true
		case apiErr.StatusCode == 408: // request timeout
			return true
		case apiErr.StatusCode == 409: // conflict (transient)
			return true
		default:
			return false // 4xx client errors are not retryable
		}
	}

	// Check for connection errors (transient network issues).
	if isConnectionError(err) {
		return true
	}

	return false
}

// isConnectionError checks if the error is a transient network error.
func isConnectionError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return false
}

// retryAfterFromError extracts a Retry-After value from an APIError if available.
func retryAfterFromError(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}
	return ""
}
