package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// Verification implements LLM-as-Judge quality re-check for REJECTED decisions.
//
// Business alignment: "标检训" (Label-Detect-Train) pipeline's "检" (Detect) stage.
// Provides false-positive control L4: independent second opinion on rejections.
//
// Core design constraints:
//   - Independence: Verifier does NOT see the original agent's reasoning or trace
//   - Only re-checks REJECTED decisions (PASSED handled by downstream monitoring)
//   - fail-closed: disagree → MANUAL_REVIEW (never PASSED), LLM failure → MANUAL_REVIEW
//   - Triggered by ReviewPlan.RequireVerification (comprehensive/high-risk pipelines)

// VerificationResult captures the LLM-as-Judge verdict.
type VerificationResult struct {
	Counterarguments []string `json:"counterarguments,omitempty"` // forced reasons the ad might be compliant
	Agree            bool     `json:"agree"`
	Reasoning        string   `json:"reasoning"`
}

// Verifier performs independent quality re-checks on REJECTED reviews.
type Verifier struct {
	client       llm.LLMClient
	store        *ReviewStore
	trainingPool *TrainingPool // nil = no training data collection
	logger       *slog.Logger
}

// NewVerifier creates a Verifier instance.
func NewVerifier(client llm.LLMClient, store *ReviewStore, logger *slog.Logger) *Verifier {
	return &Verifier{
		client: client,
		store:  store,
		logger: logger,
	}
}

// WithTrainingPool attaches a training pool for automatic data collection
// on verification overrides.
func (v *Verifier) WithTrainingPool(tp *TrainingPool) *Verifier {
	v.trainingPool = tp
	return v
}

// Verify re-checks a REJECTED review result.
//
// Flow:
//  1. Retrieve ReviewRecord from store
//  2. Build verification prompt (decision + violations + ad content only)
//  3. Call LLM (no tools)
//  4. Parse agree/disagree
//  5. Update store:
//     - agree → VerificationConfirmed, decision unchanged
//     - disagree → VerificationOverride, decision → MANUAL_REVIEW
//
// fail-closed: LLM error or parse failure → VerificationOverride + MANUAL_REVIEW.
func (v *Verifier) Verify(ctx context.Context, adID string, ad *types.AdContent) (*VerificationResult, error) {
	record, ok := v.store.Get(adID)
	if !ok {
		return nil, fmt.Errorf("record not found: %s", adID)
	}
	return v.verifyRecord(ctx, ad, record)
}

// VerifyRecord performs verification with an explicit record (useful for testing).
func (v *Verifier) VerifyRecord(ctx context.Context, ad *types.AdContent, record *ReviewRecord) (*VerificationResult, error) {
	return v.verifyRecord(ctx, ad, record)
}

func (v *Verifier) verifyRecord(ctx context.Context, ad *types.AdContent, record *ReviewRecord) (*VerificationResult, error) {
	prompt := buildVerificationPrompt(ad, record)

	maxTokens := 2048
	resp, err := v.client.ChatCompletion(ctx, types.ChatCompletionRequest{
		Messages: []types.Message{
			{Role: types.RoleSystem, Content: types.NewTextContent("You are an adversarial ad review quality auditor. Your primary goal is to identify false positives — ads that were incorrectly rejected.")},
			{Role: types.RoleUser, Content: types.NewTextContent(prompt)},
		},
		MaxTokens: &maxTokens,
	})

	if err != nil {
		v.logger.Error("verification LLM call failed",
			slog.String("ad_id", record.AdID),
			slog.String("error", err.Error()),
		)
		// fail-closed: LLM failure → disagree → MANUAL_REVIEW.
		result := &VerificationResult{Agree: false, Reasoning: fmt.Sprintf("LLM error: %s", err.Error())}
		v.applyResult(record, result)
		return result, nil
	}

	if resp == nil || len(resp.Choices) == 0 {
		result := &VerificationResult{Agree: false, Reasoning: "LLM returned empty response"}
		v.applyResult(record, result)
		return result, nil
	}

	raw := resp.Choices[0].Message.Content.String()
	vr, err := parseVerificationResult(raw)
	if err != nil {
		v.logger.Warn("verification parse failed, defaulting to disagree",
			slog.String("ad_id", record.AdID),
			slog.String("error", err.Error()),
		)
		// fail-closed: parse failure → disagree → MANUAL_REVIEW.
		result := &VerificationResult{Agree: false, Reasoning: fmt.Sprintf("parse error: %s", err.Error())}
		v.applyResult(record, result)
		return result, nil
	}

	v.applyResult(record, vr)
	return vr, nil
}

// applyResult updates the store based on the verification outcome.
func (v *Verifier) applyResult(record *ReviewRecord, vr *VerificationResult) {
	if vr.Agree {
		v.store.UpdateVerification(record.AdID, VerificationConfirmed, record.Decision, vr.Reasoning)
		v.logger.Info("verification confirmed",
			slog.String("ad_id", record.AdID),
		)
	} else {
		v.store.UpdateVerification(record.AdID, VerificationOverride, types.DecisionManualReview, vr.Reasoning)
		v.logger.Info("verification override: REJECTED → MANUAL_REVIEW",
			slog.String("ad_id", record.AdID),
			slog.String("reasoning", vr.Reasoning),
		)

		// Training data collection: verification override is a valuable signal
		// (system may have made an error — useful for model improvement).
		if v.trainingPool != nil {
			v.trainingPool.Add(&TrainingRecord{
				AdID:             record.AdID,
				OriginalDecision: record.Decision,
				FinalDecision:    types.DecisionManualReview,
				Source:           SourceVerificationOverride,
				Region:           record.Region,
				Category:         record.Category,
				Confidence:       record.Confidence,
			})
		}
	}
}

// buildVerificationPrompt constructs an adversarial verification prompt.
//
// Anti-confirmation-bias design:
//   - Forces the verifier to generate counterarguments BEFORE judging
//   - Pre-debunks common rationalization patterns that lead to rubber-stamping
//   - Requires specific evidence for each violation (not vague interpretations)
//
// CRITICAL: Does NOT include reasoning or agent_trace — independence preserved.
func buildVerificationPrompt(ad *types.AdContent, record *ReviewRecord) string {
	var b strings.Builder

	// 1. Adversarial framing.
	b.WriteString("You are an adversarial reviewer. Your job is to find reasons why this ")
	b.WriteString("REJECTED decision might be WRONG. You are the advertiser's last line of defense ")
	b.WriteString("against false positives.\n\n")

	// 2. Ad content — all fields for independent analysis.
	b.WriteString("=== AD CONTENT ===\n")
	fmt.Fprintf(&b, "Ad ID: %s\n", ad.ID)
	fmt.Fprintf(&b, "Type: %s\n", ad.Type)
	fmt.Fprintf(&b, "Region: %s\n", ad.Region)
	fmt.Fprintf(&b, "Category: %s\n", ad.Category)
	fmt.Fprintf(&b, "Advertiser: %s\n\n", ad.AdvertiserID)

	fmt.Fprintf(&b, "Headline: %s\n", ad.Content.Headline)
	fmt.Fprintf(&b, "Body: %s\n", ad.Content.Body)
	if ad.Content.CTA != "" {
		fmt.Fprintf(&b, "CTA: %s\n", ad.Content.CTA)
	}
	if ad.Content.ImageDescription != "" {
		fmt.Fprintf(&b, "Image Description: %s\n", ad.Content.ImageDescription)
	}

	fmt.Fprintf(&b, "\nLanding Page: %s\n", ad.LandingPage.URL)
	fmt.Fprintf(&b, "Landing Page Description: %s\n", ad.LandingPage.Description)
	fmt.Fprintf(&b, "Accessible: %v\n", ad.LandingPage.IsAccessible)
	fmt.Fprintf(&b, "Mobile Optimized: %v\n\n", ad.LandingPage.IsMobileOptimized)

	// 3. Review decision and violations — NO reasoning, NO agent_trace.
	b.WriteString("=== REVIEW DECISION ===\n")
	fmt.Fprintf(&b, "Decision: %s\n", record.Decision)
	fmt.Fprintf(&b, "Confidence: %.2f\n", record.Confidence)

	if len(record.Violations) > 0 {
		b.WriteString("\nViolations found:\n")
		for _, v := range record.Violations {
			fmt.Fprintf(&b, "- [%s] severity=%s: %s (confidence=%.2f", v.PolicyID, v.Severity, v.Description, v.Confidence)
			if v.Evidence != "" {
				fmt.Fprintf(&b, ", evidence=%q", v.Evidence)
			}
			b.WriteString(")\n")
		}
	} else {
		b.WriteString("\nNo specific violations reported.\n")
	}

	// 4. Forced counterarguments.
	b.WriteString("\n=== REQUIRED ANALYSIS ===\n")
	b.WriteString("Before making your judgment, you MUST list at least 3 reasons why this ad ")
	b.WriteString("might actually be COMPLIANT and the REJECTED decision might be WRONG.\n\n")

	// 5. Pre-debunked rationalizations.
	b.WriteString("=== INVALID RATIONALIZATIONS ===\n")
	b.WriteString("The following arguments are NOT valid reasons to uphold a rejection:\n")
	b.WriteString("- \"The policy is broadly worded\" — Broad interpretation causes false positives. ")
	b.WriteString("The ad must specifically violate the policy, not merely touch the policy's domain.\n")
	b.WriteString("- \"Better safe than sorry\" — False positives mean revenue loss and advertiser churn. ")
	b.WriteString("Safety requires accuracy, not over-rejection.\n")
	b.WriteString("- \"The advertiser has a history of violations\" — Each ad is judged independently ")
	b.WriteString("on its own content. Past behavior is not evidence of current violation.\n")
	b.WriteString("- \"The severity is critical\" — Severity does not make a wrong decision right. ")
	b.WriteString("A critical-severity policy that is not actually violated is still not violated.\n\n")

	// 6. Evidence standard.
	b.WriteString("=== EVIDENCE STANDARD ===\n")
	b.WriteString("Each violation must cite SPECIFIC TEXT from the ad that violates the policy. ")
	b.WriteString("If a violation cannot point to specific ad text as evidence, it is insufficient.\n\n")

	// 7. Output format with counterarguments.
	b.WriteString("=== OUTPUT ===\n")
	b.WriteString("Output ONLY a JSON object:\n")
	b.WriteString(`{"counterarguments":["reason1","reason2","reason3"],"agree":true/false,"reasoning":"..."}`)
	b.WriteString("\n\n")
	b.WriteString("- counterarguments: at least 3 reasons the ad might be compliant (REQUIRED)\n")
	b.WriteString("- agree: true ONLY if violations are valid despite your counterarguments\n")
	b.WriteString("- agree: false if counterarguments reveal the rejection is unjustified\n")

	return b.String()
}

// parseVerificationResult extracts agree/disagree from LLM output.
func parseVerificationResult(raw string) (*VerificationResult, error) {
	raw = strings.TrimSpace(raw)

	// Try direct JSON parse.
	var vr VerificationResult
	if err := json.Unmarshal([]byte(raw), &vr); err == nil {
		return &vr, nil
	}

	// Try extracting JSON from markdown fences.
	if idx := strings.Index(raw, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(raw[start:], "```"); end >= 0 {
			candidate := strings.TrimSpace(raw[start : start+end])
			if err := json.Unmarshal([]byte(candidate), &vr); err == nil {
				return &vr, nil
			}
		}
	}

	// Try first {...} block.
	if start := strings.Index(raw, "{"); start >= 0 {
		if end := strings.LastIndex(raw, "}"); end > start {
			candidate := raw[start : end+1]
			if err := json.Unmarshal([]byte(candidate), &vr); err == nil {
				return &vr, nil
			}
		}
	}

	return nil, fmt.Errorf("no valid JSON found in verification output: %s", truncate(raw, 200))
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
