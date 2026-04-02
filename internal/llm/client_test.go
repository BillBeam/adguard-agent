package llm

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// TestChatCompletion_Smoke 是 LLM Client 的冒烟测试，需要 LLM_API_KEY 环境变量。
// 跳过条件：未设置 API KEY 时自动 skip。
func TestChatCompletion_Smoke(t *testing.T) {
	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		t.Skip("LLM_API_KEY not set, skipping smoke test")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	client, err := NewClient(ProviderConfig{
		Name:       "xai",
		BaseURL:    "https://api.x.ai/v1",
		APIKey:     apiKey,
		Model:      "grok-3-mini-beta",
		MaxRetries: 3,
		Timeout:    30 * time.Second,
	}, logger)
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.ChatCompletion(ctx, types.ChatCompletionRequest{
		Messages: []types.Message{
			{
				Role:    types.RoleUser,
				Content: types.NewTextContent("Reply with exactly one word: hello"),
			},
		},
	})
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}

	t.Logf("Model: %s", resp.Model)
	t.Logf("Response: %s", resp.Choices[0].Message.Content.String())
	t.Logf("Usage: %d prompt + %d completion tokens", resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	t.Logf("Session cost: $%.6f", client.Usage().TotalCost())
}
