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

// withRetry executes an operation with exponential backoff retry.
//
// The retry strategy classifies errors by HTTP status code:
//   - 429 (rate limit): retry with Retry-After header if present
//   - 529 (overloaded): retry up to maxRetries
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
	maxRetries int,
	operation func(ctx context.Context, attempt int) (T, error),
) (T, error) {
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries+1; attempt++ {
		result, err := operation(ctx, attempt)
		if err == nil {
			return result, nil
		}
		lastErr = err

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
