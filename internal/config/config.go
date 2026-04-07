// Package config implements three-tier configuration loading.
//
// Precedence (highest to lowest):
//  1. Environment variables (LLM_PROVIDER, LLM_API_KEY, LLM_BASE_URL, LLM_MODEL, LOG_LEVEL, DATA_DIR)
//  2. Config file (config.json in working directory)
//  3. Built-in defaults
//
// Defaults are populated first, then JSON.Unmarshal overlays only the fields
// present in the file, then environment variables override individual fields.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Config is the top-level configuration for AdGuard Agent.
type Config struct {
	LLM     LLMConfig     `json:"llm"`
	Data    DataConfig    `json:"data"`
	Logging LoggingConfig `json:"logging"`
}

// LLMConfig holds LLM provider settings.
type LLMConfig struct {
	Provider   string        `json:"provider"`
	BaseURL    string        `json:"base_url"`
	APIKey     string        `json:"api_key,omitempty"`
	Model      string        `json:"model"`
	MaxRetries int           `json:"max_retries"`
	Timeout    time.Duration `json:"timeout"`
}

// DataConfig specifies paths to data files.
type DataConfig struct {
	Dir              string `json:"dir"`
	PolicyKBFile     string `json:"policy_kb_file"`
	RegionRulesFile  string `json:"region_rules_file"`
	CategoryRiskFile string `json:"category_risk_file"`
	SamplesFile      string `json:"samples_file"`
}

// LoggingConfig controls log output.
type LoggingConfig struct {
	Level string `json:"level"`
}

// defaultConfig returns the built-in default configuration.
func defaultConfig() Config {
	return Config{
		LLM: LLMConfig{
			Provider:   "xai",
			BaseURL:    "https://api.x.ai/v1",
			Model:      "grok-4-1-fast-reasoning",
			MaxRetries: 10,
			Timeout:    10 * time.Minute,
		},
		Data: DataConfig{
			Dir:              "data",
			PolicyKBFile:     "policy_kb.json",
			RegionRulesFile:  "region_rules.json",
			CategoryRiskFile: "category_risk.json",
			SamplesFile:      "samples/all_samples.json",
		},
		Logging: LoggingConfig{
			Level: "warn", // clean demo output; use LOG_LEVEL=info for verbose
		},
	}
}

// LoadConfig loads configuration using the three-tier merge strategy.
func LoadConfig() (*Config, error) {
	cfg := defaultConfig()

	// Tier 2: overlay config file (if it exists).
	if err := loadConfigFile("config.json", &cfg); err != nil {
		return nil, fmt.Errorf("loading config file: %w", err)
	}

	// Tier 1: override with environment variables (highest priority).
	applyEnvOverrides(&cfg)

	return &cfg, nil
}

// loadConfigFile reads a JSON config file and unmarshals onto the existing config.
// Go's json.Unmarshal only overwrites fields present in the JSON, preserving defaults.
// If the file doesn't exist, this is a no-op (silent fallback to defaults).
func loadConfigFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no config file is fine — use defaults
		}
		return fmt.Errorf("reading %s: %w", path, err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

// applyEnvOverrides reads environment variables and overrides config fields.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("LLM_PROVIDER"); v != "" {
		cfg.LLM.Provider = v
	}
	if v := os.Getenv("LLM_API_KEY"); v != "" {
		cfg.LLM.APIKey = v
	}
	if v := os.Getenv("LLM_BASE_URL"); v != "" {
		cfg.LLM.BaseURL = v
	}
	if v := os.Getenv("LLM_MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := os.Getenv("LLM_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.LLM.MaxRetries = n
		}
	}
	if v := os.Getenv("LLM_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.LLM.Timeout = d
		}
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("DATA_DIR"); v != "" {
		cfg.Data.Dir = v
	}
}

// ParseLogLevel converts a string log level to slog.Level.
func ParseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
