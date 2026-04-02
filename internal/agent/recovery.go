package agent

import (
	"fmt"
	"log/slog"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// handleMaxOutputTokens implements two-level max_output_tokens recovery.
//
// Level 1: Escalate token limit (8k → 64k), one-time only.
// Level 2: Inject "continue" recovery message, up to MaxRecoveryAttempts times.
//
// Returns true if recovery was applied (caller should continue the loop).
// Returns false if recovery is exhausted (caller should fallback to MANUAL_REVIEW).
func handleMaxOutputTokens(state *State, config *LoopConfig, logger *slog.Logger) bool {
	// Level 1: Escalate (once only).
	if state.MaxTokensOverride == nil {
		escalated := config.EscalatedMaxTokens
		state.MaxTokensOverride = &escalated
		state.Transition(StateAnalyzing, TransitionMaxOutputEscalate,
			fmt.Sprintf("escalating max_tokens from %d to %d", config.DefaultMaxTokens, escalated))
		logger.Info("max_output_tokens: escalating",
			slog.Int("new_limit", escalated),
		)
		return true
	}

	// Level 2: Inject recovery message (up to MaxRecoveryAttempts).
	if state.MaxOutputRecoveryCount < config.MaxRecoveryAttempts {
		state.MaxOutputRecoveryCount++
		state.AppendMessage(types.Message{
			Role: types.RoleUser,
			Content: types.NewTextContent(
				"Output token limit reached. Resume your analysis directly from where you stopped. " +
					"Do not apologize or summarize what you already said. Continue with the next step."),
		})
		state.Transition(StateAnalyzing, TransitionMaxOutputRecovery,
			fmt.Sprintf("recovery attempt %d/%d", state.MaxOutputRecoveryCount, config.MaxRecoveryAttempts))
		logger.Info("max_output_tokens: recovery",
			slog.Int("attempt", state.MaxOutputRecoveryCount),
			slog.Int("max", config.MaxRecoveryAttempts),
		)
		return true
	}

	// Recovery exhausted.
	logger.Warn("max_output_tokens: recovery exhausted",
		slog.Int("attempts", state.MaxOutputRecoveryCount),
	)
	return false
}
