package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// LLMClient is the interface for interacting with LLM APIs.
// All providers (xAI, OpenAI, etc.) implement this interface via the
// OpenAI-compatible /v1/chat/completions endpoint.
type LLMClient interface {
	// ChatCompletion sends a non-streaming completion request.
	ChatCompletion(ctx context.Context, req types.ChatCompletionRequest) (*types.ChatCompletionResponse, error)

	// StreamChatCompletion sends a streaming completion request and returns
	// a reader for incrementally consuming SSE chunks.
	StreamChatCompletion(ctx context.Context, req types.ChatCompletionRequest) (*StreamReader, error)

	// Usage returns the session usage tracker for cost monitoring.
	Usage() *SessionUsage
}

// httpClient implements LLMClient using net/http.
// Factory pattern: NewClient creates this based on ProviderConfig.
type httpClient struct {
	client   *http.Client
	config   ProviderConfig
	logger   *slog.Logger
	usage    *SessionUsage
}

// NewClient creates an LLMClient for the configured provider.
// The client uses OpenAI-compatible /v1/chat/completions for all providers.
func NewClient(cfg ProviderConfig, logger *slog.Logger) (LLMClient, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("LLM API key is required (set LLM_API_KEY environment variable)")
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("LLM base URL is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &httpClient{
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
		config: cfg,
		logger: logger,
		usage:  NewSessionUsage(),
	}, nil
}

// ChatCompletion sends a non-streaming request with retry.
func (c *httpClient) ChatCompletion(ctx context.Context, req types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	// Ensure model is set.
	if req.Model == "" {
		req.Model = c.config.Model
	}
	req.Stream = false

	return withRetry(ctx, c.logger, c.config.MaxRetries, func(ctx context.Context, attempt int) (*types.ChatCompletionResponse, error) {
		resp, err := c.doRequest(ctx, req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading response body: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, c.parseAPIError(resp.StatusCode, body, resp.Header)
		}

		var result types.ChatCompletionResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("parsing response JSON: %w", err)
		}

		// Track usage.
		if result.Usage != nil {
			c.usage.Add(result.Model, *result.Usage)
		}

		c.logger.Debug("API call completed",
			slog.String("model", result.Model),
			slog.Int("prompt_tokens", safeUsageField(result.Usage, true)),
			slog.Int("completion_tokens", safeUsageField(result.Usage, false)),
		)

		return &result, nil
	})
}

// StreamChatCompletion sends a streaming request and returns a StreamReader.
// The caller must call reader.Close() when done.
func (c *httpClient) StreamChatCompletion(ctx context.Context, req types.ChatCompletionRequest) (*StreamReader, error) {
	if req.Model == "" {
		req.Model = c.config.Model
	}
	req.Stream = true

	resp, err := c.doRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, c.parseAPIError(resp.StatusCode, body, resp.Header)
	}

	return &StreamReader{
		scanner: bufio.NewScanner(resp.Body),
		body:    resp.Body,
		model:   req.Model,
		usage:   c.usage,
	}, nil
}

// Usage returns the session usage tracker.
func (c *httpClient) Usage() *SessionUsage {
	return c.usage
}

// doRequest builds and sends the HTTP request to the API endpoint.
// Attaches authorization, content-type, and per-request tracing headers.
func (c *httpClient) doRequest(ctx context.Context, req types.ChatCompletionRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	url := strings.TrimRight(c.config.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}

	// Standard headers.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.config.APIKey)

	// Per-request tracing ID — survives timeouts where server request IDs are unavailable.
	httpReq.Header.Set("X-Client-Request-ID", generateRequestID())

	// Custom headers from provider config.
	for k, v := range c.config.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	return resp, nil
}

// parseAPIError creates a structured APIError from an HTTP error response.
func (c *httpClient) parseAPIError(statusCode int, body []byte, headers http.Header) *APIError {
	msg := string(body)

	// Try to extract a more specific error message from JSON response.
	var errResp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Error.Message != "" {
		msg = errResp.Error.Message
	}

	apiErr := &APIError{
		StatusCode: statusCode,
		Message:    msg,
	}

	// Preserve Retry-After header for the retry logic.
	if ra := headers.Get("Retry-After"); ra != "" {
		apiErr.RetryAfter = ra
	}

	return apiErr
}

// generateRequestID creates a UUID v4 for request tracing.
func generateRequestID() string {
	var uuid [16]byte
	_, _ = rand.Read(uuid[:])
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:])
}

func safeUsageField(u *types.Usage, isInput bool) int {
	if u == nil {
		return 0
	}
	if isInput {
		return u.PromptTokens
	}
	return u.CompletionTokens
}

// --- StreamReader ---

// StreamReader incrementally reads SSE chunks from a streaming response.
// Implements the "data: {json}\n\n" SSE protocol with "data: [DONE]" termination.
type StreamReader struct {
	scanner *bufio.Scanner
	body    io.ReadCloser
	model   string
	usage   *SessionUsage
}

// Recv reads the next chunk from the SSE stream.
// Returns io.EOF when the stream is complete (after "data: [DONE]").
func (r *StreamReader) Recv() (*types.ChatCompletionChunk, error) {
	for r.scanner.Scan() {
		line := r.scanner.Text()

		// Skip empty lines and SSE comments.
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		// Strip "data: " prefix.
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		// Check for stream termination.
		if data == "[DONE]" {
			return nil, io.EOF
		}

		var chunk types.ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("parsing SSE chunk: %w", err)
		}

		// Track usage from final chunk (if included).
		if chunk.Usage != nil {
			r.usage.Add(chunk.Model, *chunk.Usage)
		}

		return &chunk, nil
	}

	if err := r.scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading SSE stream: %w", err)
	}
	return nil, io.EOF
}

// Close releases the underlying HTTP response body.
func (r *StreamReader) Close() error {
	return r.body.Close()
}
