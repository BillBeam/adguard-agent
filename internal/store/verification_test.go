package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// mockVerifyLLM is a minimal LLM client for verification tests.
type mockVerifyLLM struct {
	response *types.ChatCompletionResponse
	err      error
	lastReq  *types.ChatCompletionRequest
}

func (m *mockVerifyLLM) ChatCompletion(_ context.Context, req types.ChatCompletionRequest) (*types.ChatCompletionResponse, error) {
	m.lastReq = &req
	return m.response, m.err
}

func (m *mockVerifyLLM) StreamChatCompletion(_ context.Context, _ types.ChatCompletionRequest) (*llm.StreamReader, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockVerifyLLM) Usage() *llm.SessionUsage { return llm.NewSessionUsage() }

func testAd() *types.AdContent {
	return &types.AdContent{
		ID:           "ad_test",
		Type:         "text",
		Region:       "US",
		Category:     "healthcare",
		AdvertiserID: "adv_001",
		Content: types.AdBody{
			Headline: "Miracle Cure for Diabetes",
			Body:     "100% effective, FDA approved",
			CTA:      "Buy Now",
		},
		LandingPage: types.LandingPage{
			URL:         "https://example.com",
			Description: "Product page",
			IsAccessible: true,
		},
	}
}

func testRecord() *ReviewRecord {
	return &ReviewRecord{
		ReviewResult: types.ReviewResult{
			AdID:       "ad_test",
			Decision:   types.DecisionRejected,
			Confidence: 0.88,
			Violations: []types.PolicyViolation{
				{PolicyID: "POL_001", Severity: "critical", Description: "Unverified medical claim", Confidence: 0.90, Evidence: "Miracle Cure for Diabetes"},
			},
		},
		AdvertiserID: "adv_001",
		Region:       "US",
		Category:     "healthcare",
	}
}

func TestVerifier_Agree(t *testing.T) {
	mockLLM := &mockVerifyLLM{
		response: &types.ChatCompletionResponse{
			Choices: []types.Choice{{
				Message: types.Message{
					Role:    types.RoleAssistant,
					Content: types.NewTextContent(`{"agree": true, "reasoning": "The ad makes unverified medical claims"}`),
				},
				FinishReason: "stop",
			}},
		},
	}

	rs := NewReviewStore(testLogger(), "")
	record := testRecord()
	rs.Store(record)

	v := NewVerifier(mockLLM, rs, testLogger())
	result, err := v.Verify(context.Background(), "ad_test", testAd())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Agree {
		t.Error("expected agree=true")
	}

	// Check store was updated.
	got, _ := rs.Get("ad_test")
	if got.VerificationStatus != VerificationConfirmed {
		t.Errorf("expected confirmed status, got %s", got.VerificationStatus)
	}
}

func TestVerifier_Disagree(t *testing.T) {
	mockLLM := &mockVerifyLLM{
		response: &types.ChatCompletionResponse{
			Choices: []types.Choice{{
				Message: types.Message{
					Role:    types.RoleAssistant,
					Content: types.NewTextContent(`{"agree": false, "reasoning": "The violations are not well substantiated"}`),
				},
				FinishReason: "stop",
			}},
		},
	}

	rs := NewReviewStore(testLogger(), "")
	rs.Store(testRecord())

	v := NewVerifier(mockLLM, rs, testLogger())
	result, err := v.Verify(context.Background(), "ad_test", testAd())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Agree {
		t.Error("expected agree=false")
	}

	// Check store: should be override → MANUAL_REVIEW.
	got, _ := rs.Get("ad_test")
	if got.VerificationStatus != VerificationOverride {
		t.Errorf("expected override status, got %s", got.VerificationStatus)
	}
	if got.VerifiedDecision != types.DecisionManualReview {
		t.Errorf("expected MANUAL_REVIEW, got %s", got.VerifiedDecision)
	}
}

func TestVerifier_LLMFailure(t *testing.T) {
	mockLLM := &mockVerifyLLM{err: fmt.Errorf("API error")}

	rs := NewReviewStore(testLogger(), "")
	rs.Store(testRecord())

	v := NewVerifier(mockLLM, rs, testLogger())
	result, err := v.Verify(context.Background(), "ad_test", testAd())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// fail-closed: LLM failure → disagree → MANUAL_REVIEW.
	if result.Agree {
		t.Error("expected disagree on LLM failure (fail-closed)")
	}

	got, _ := rs.Get("ad_test")
	if got.VerificationStatus != VerificationOverride {
		t.Errorf("expected override on LLM failure, got %s", got.VerificationStatus)
	}
}

func TestVerifier_ParseFailure(t *testing.T) {
	mockLLM := &mockVerifyLLM{
		response: &types.ChatCompletionResponse{
			Choices: []types.Choice{{
				Message: types.Message{
					Role:    types.RoleAssistant,
					Content: types.NewTextContent("I think the ad is fine but I can't format JSON"),
				},
				FinishReason: "stop",
			}},
		},
	}

	rs := NewReviewStore(testLogger(), "")
	rs.Store(testRecord())

	v := NewVerifier(mockLLM, rs, testLogger())
	result, err := v.Verify(context.Background(), "ad_test", testAd())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// fail-closed: parse failure → disagree → MANUAL_REVIEW.
	if result.Agree {
		t.Error("expected disagree on parse failure (fail-closed)")
	}
}

func TestVerifier_PromptIndependence(t *testing.T) {
	mockLLM := &mockVerifyLLM{
		response: &types.ChatCompletionResponse{
			Choices: []types.Choice{{
				Message: types.Message{
					Role:    types.RoleAssistant,
					Content: types.NewTextContent(`{"agree": true, "reasoning": "valid"}`),
				},
				FinishReason: "stop",
			}},
		},
	}

	rs := NewReviewStore(testLogger(), "")
	record := testRecord()
	record.AgentTrace = []string{"tool_call:analyze_content", "tool_result:analyze_content", "LLM final judgment"}
	rs.Store(record)

	v := NewVerifier(mockLLM, rs, testLogger())
	v.Verify(context.Background(), "ad_test", testAd())

	// Verify the prompt sent to LLM does NOT contain reasoning or agent_trace.
	if mockLLM.lastReq == nil {
		t.Fatal("expected LLM to be called")
	}

	// Check user message (index 1) for absence of agent trace content.
	userMsg := mockLLM.lastReq.Messages[1].Content.String()
	if strings.Contains(userMsg, "tool_call:analyze_content") {
		t.Error("prompt should NOT contain agent_trace (independence violation)")
	}
	if strings.Contains(userMsg, "reasoning") && strings.Contains(userMsg, "LLM final judgment") {
		t.Error("prompt should NOT contain LLM reasoning (independence violation)")
	}

	// But should contain the ad content and violations.
	if !strings.Contains(userMsg, "Miracle Cure for Diabetes") {
		t.Error("prompt should contain ad headline")
	}
	if !strings.Contains(userMsg, "POL_001") {
		t.Error("prompt should contain policy violation")
	}
}

func TestVerifier_RecordNotFound(t *testing.T) {
	mockLLM := &mockVerifyLLM{}
	rs := NewReviewStore(testLogger(), "")

	v := NewVerifier(mockLLM, rs, testLogger())
	_, err := v.Verify(context.Background(), "nonexistent", testAd())
	if err == nil {
		t.Error("expected error for nonexistent record")
	}
}
