package llm

import (
	"os"
	"strconv"
	"time"
)

// ProviderConfig holds the configuration for an LLM API provider.
// Populated from config.Config or directly from environment variables.
type ProviderConfig struct {
	Name       string            // provider identifier, e.g. "xai", "openai"
	BaseURL    string            // API endpoint, e.g. "https://api.x.ai/v1"
	APIKey     string            // bearer token for Authorization header
	Model      string            // default model name
	MaxRetries int               // max retry attempts for transient errors
	Timeout    time.Duration     // HTTP client timeout
	Headers    map[string]string // additional custom headers
}

// DefaultProviderConfig builds a ProviderConfig from environment variables
// with sensible defaults matching the xAI provider.
func DefaultProviderConfig() ProviderConfig {
	return ProviderConfig{
		Name:       envOr("LLM_PROVIDER", "xai"),
		BaseURL:    envOr("LLM_BASE_URL", "https://api.x.ai/v1"),
		APIKey:     os.Getenv("LLM_API_KEY"),
		Model:      envOr("LLM_MODEL", "grok-4-1-fast-reasoning"),
		MaxRetries: envIntOr("LLM_MAX_RETRIES", 10),
		Timeout:    envDurationOr("LLM_TIMEOUT", 10*time.Minute),
	}
}

// envOr returns the environment variable value or the default.
func envOr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// envIntOr returns the environment variable as an int or the default.
func envIntOr(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultVal
}

// envDurationOr returns the environment variable as a duration or the default.
func envDurationOr(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultVal
}
