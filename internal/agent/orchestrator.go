package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/strategy"
	"github.com/BillBeam/adguard-agent/internal/tool"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// Orchestrator implements Multi-Agent ad review.
//
// Key design decisions:
//   - Sub-agents reuse the same Run() function with different configs
//   - Each sub-agent has isolated State (messages, transition log)
//   - Shared: LLM Client, StrategyMatrix
//   - Parallel execution via goroutines + sync.WaitGroup
//
// Execution flow for standard/comprehensive pipelines:
//  1. Fork 3 specialist agents (ContentAgent, PolicyAgent, RegionAgent) in parallel
//  2. Collect AgentResults from all 3
//  3. Run AdjudicatorAgent with collected results as input
//  4. Return final ReviewResult with full QueryChain
type Orchestrator struct {
	client   llm.LLMClient
	matrix   *strategy.StrategyMatrix
	registry *tool.Registry
	logger   *slog.Logger
}

// NewOrchestrator creates a Multi-Agent orchestrator.
func NewOrchestrator(
	client llm.LLMClient,
	matrix *strategy.StrategyMatrix,
	registry *tool.Registry,
	logger *slog.Logger,
) *Orchestrator {
	return &Orchestrator{
		client:   client,
		matrix:   matrix,
		registry: registry,
		logger:   logger,
	}
}

// AgentResult captures one specialist agent's output.
// Parsed from the agent's final JSON output via Run().
type AgentResult struct {
	Role       AgentRole              `json:"agent"`
	Decision   string                 `json:"decision"`
	Confidence float64                `json:"confidence"`
	Violations []types.PolicyViolation `json:"violations"`
	Reasoning  string                 `json:"reasoning"`
	Duration   time.Duration          `json:"duration"`
	Trace      []string               `json:"trace"`
	ExitReason ExitReason             `json:"exit_reason"`
}

// MultiAgentResult is the combined output of the Multi-Agent review.
type MultiAgentResult struct {
	ReviewResult *types.ReviewResult `json:"review_result"`
	AgentResults []AgentResult       `json:"agent_results"`
	ChainLog     *ChainLog           `json:"chain_log"`
}

// RunMultiAgent executes the full Multi-Agent review pipeline.
//
// Flow:
//  1. Create QueryChain for tracking
//  2. Fork 3 specialist agents in parallel (goroutines)
//  3. Collect and parse results
//  4. Run Adjudicator with collected results
//  5. Build final ReviewResult
func (o *Orchestrator) RunMultiAgent(ctx context.Context, ad *types.AdContent, plan types.ReviewPlan) (*MultiAgentResult, error) {
	start := time.Now()
	chain := NewQueryChain()
	chainLog := NewChainLog(chain.ChainID)

	policies := o.matrix.GetApplicablePolicies(ad.Region, ad.Category)

	o.logger.Info("multi-agent review started",
		slog.String("ad_id", ad.ID),
		slog.String("pipeline", plan.Pipeline),
		slog.String("chain_id", chain.ChainID[:8]),
		slog.Int("specialists", len(SpecialistAgentSpecs(plan.Pipeline))),
	)

	// Phase 1: Fork specialist agents in parallel.
	specs := SpecialistAgentSpecs(plan.Pipeline)
	agentResults := o.runSpecialists(ctx, ad, policies, plan, specs, chain, chainLog)

	o.logger.Info("specialists completed",
		slog.String("ad_id", ad.ID),
		slog.Int("results", len(agentResults)),
	)

	// Phase 2: Run Adjudicator.
	adjResult := o.runAdjudicator(ctx, ad, agentResults, plan, chain, chainLog)

	// Phase 3: Build final ReviewResult from Adjudicator output.
	result := o.buildFinalResult(ad, adjResult, agentResults, start)

	return &MultiAgentResult{
		ReviewResult: result,
		AgentResults: agentResults,
		ChainLog:     chainLog,
	}, nil
}

// runSpecialists forks and runs all specialist agents in parallel.
func (o *Orchestrator) runSpecialists(
	ctx context.Context,
	ad *types.AdContent,
	policies []types.Policy,
	plan types.ReviewPlan,
	specs []AgentSpec,
	chain *QueryChain,
	chainLog *ChainLog,
) []AgentResult {
	results := make([]AgentResult, len(specs))
	var wg sync.WaitGroup
	wg.Add(len(specs))

	for i, spec := range specs {
		go func(idx int, s AgentSpec) {
			defer wg.Done()
			results[idx] = o.runSingleAgent(ctx, ad, policies, plan, s, chain.Child(), chainLog)
		}(i, spec)
	}

	wg.Wait()
	return results
}

// runSingleAgent executes one specialist agent via Run().
// Key design: reuses the SAME Run() function as Phase 2's single-agent path.
func (o *Orchestrator) runSingleAgent(
	ctx context.Context,
	ad *types.AdContent,
	policies []types.Policy,
	plan types.ReviewPlan,
	spec AgentSpec,
	childChain *QueryChain,
	chainLog *ChainLog,
) AgentResult {
	start := time.Now()

	// Build isolated config for this agent.
	subRegistry := o.registry.Sub(spec.Tools...)
	subExecutor := tool.NewExecutor(subRegistry, o.logger)

	config := &LoopConfig{
		MaxTurns:            spec.MaxTurns,
		ConfidenceThreshold: plan.ConfidenceThreshold,
		AllowAutoReject:     plan.AllowAutoReject,
		RequireVerification: false, // Verification happens at orchestrator level
		Pipeline:            plan.Pipeline,
		Tools:               subRegistry.ExportDefinitions(),
		ToolExecutor:        subExecutor,
		SystemPrompt:        BuildAgentSystemPrompt(spec.Role, ad, policies, plan),
		DefaultMaxTokens:    DefaultMaxOutputTokens,
		EscalatedMaxTokens:  EscalatedMaxOutputTokens,
		MaxRecoveryAttempts: MaxRecoveryAttempts,
	}

	// Build isolated state — each agent starts with fresh messages.
	state := NewState(ad)
	state.Messages = buildInitialMessages(config.SystemPrompt)

	o.logger.Debug("specialist agent started",
		slog.String("role", string(spec.Role)),
		slog.String("ad_id", ad.ID),
		slog.Int("tools", len(spec.Tools)),
		slog.Int("max_turns", spec.MaxTurns),
	)

	// Execute — reuses the same Run() function as single-agent path.
	loopResult := Run(ctx, o.client, config, state, nil, o.logger)

	duration := time.Since(start)
	ar := parseAgentResult(spec.Role, loopResult, state, duration)

	// Record in chain log.
	chainLog.Add(ChainEntry{
		ChainID:     childChain.ChainID,
		Depth:       childChain.Depth,
		AgentRole:   string(spec.Role),
		Decision:    ar.Decision,
		Confidence:  ar.Confidence,
		ToolsCalled: extractToolsCalled(state),
		Duration:    duration,
		Trace:       ar.Trace,
	})

	o.logger.Info("specialist agent completed",
		slog.String("role", string(spec.Role)),
		slog.String("decision", ar.Decision),
		slog.Float64("confidence", ar.Confidence),
		slog.Duration("duration", duration),
	)

	return ar
}

// runAdjudicator runs the Adjudicator agent to synthesize specialist results.
func (o *Orchestrator) runAdjudicator(
	ctx context.Context,
	ad *types.AdContent,
	agentResults []AgentResult,
	plan types.ReviewPlan,
	chain *QueryChain,
	chainLog *ChainLog,
) AgentResult {
	start := time.Now()
	spec := AdjudicatorSpec(plan.Pipeline)
	childChain := chain.Child()

	// Adjudicator has no tools — pure reasoning.
	config := &LoopConfig{
		MaxTurns:            spec.MaxTurns,
		ConfidenceThreshold: plan.ConfidenceThreshold,
		AllowAutoReject:     plan.AllowAutoReject,
		Pipeline:            plan.Pipeline,
		Tools:               nil, // No tools for Adjudicator
		ToolExecutor:        &noOpExecutor{},
		SystemPrompt:        BuildAdjudicatorPrompt(ad, agentResults, plan),
		DefaultMaxTokens:    DefaultMaxOutputTokens,
		EscalatedMaxTokens:  EscalatedMaxOutputTokens,
		MaxRecoveryAttempts: MaxRecoveryAttempts,
	}

	state := NewState(ad)
	state.Messages = []types.Message{
		{Role: types.RoleSystem, Content: types.NewTextContent(config.SystemPrompt)},
		{Role: types.RoleUser, Content: types.NewTextContent(
			"Based on the specialist agent reports above, provide your final review decision as JSON.")},
	}

	loopResult := Run(ctx, o.client, config, state, nil, o.logger)

	duration := time.Since(start)
	ar := parseAgentResult(RoleAdjudicator, loopResult, state, duration)

	chainLog.Add(ChainEntry{
		ChainID:   childChain.ChainID,
		Depth:     childChain.Depth,
		AgentRole: "adjudicator",
		Decision:  ar.Decision,
		Confidence: ar.Confidence,
		Duration:  duration,
		Trace:     ar.Trace,
	})

	o.logger.Info("adjudicator completed",
		slog.String("decision", ar.Decision),
		slog.Float64("confidence", ar.Confidence),
	)

	return ar
}

// buildFinalResult constructs the final ReviewResult from the Adjudicator's output,
// with fallback to programmatic aggregation if Adjudicator parsing fails.
func (o *Orchestrator) buildFinalResult(
	ad *types.AdContent,
	adjResult AgentResult,
	agentResults []AgentResult,
	startTime time.Time,
) *types.ReviewResult {
	// Collect all violations from all agents (deduplicated by policy_id).
	allViolations := mergeViolations(agentResults)

	// Determine risk level from violations.
	riskLevel := types.RiskMedium
	for _, v := range allViolations {
		if v.Severity == "critical" {
			riskLevel = types.RiskCritical
			break
		}
		if v.Severity == "high" {
			riskLevel = types.RiskHigh
		}
	}

	// Build agent trace.
	trace := buildMultiAgentTrace(agentResults, adjResult)

	// Use Adjudicator's decision if valid, otherwise fall back to programmatic aggregation.
	decision := types.ReviewDecision(adjResult.Decision)
	confidence := adjResult.Confidence

	switch decision {
	case types.DecisionPassed, types.DecisionRejected, types.DecisionManualReview:
		// Valid decision from Adjudicator.
	default:
		// Adjudicator failed — use programmatic L3 aggregation.
		o.logger.Warn("adjudicator decision invalid, using programmatic aggregation",
			slog.String("raw_decision", adjResult.Decision))
		decision, confidence = aggregateDecisions(agentResults)
	}

	// L3 false-positive control: apply regardless of Adjudicator's output.
	decision, confidence = applyL3Control(agentResults, decision, confidence)

	return &types.ReviewResult{
		AdID:           ad.ID,
		Decision:       decision,
		Confidence:     confidence,
		Violations:     allViolations,
		RiskLevel:      riskLevel,
		AgentTrace:     trace,
		ReviewDuration: time.Since(startTime),
		Timestamp:      time.Now(),
	}
}

// --- Helper functions ---

// parseAgentResult extracts an AgentResult from a LoopResult.
func parseAgentResult(role AgentRole, lr *LoopResult, state *State, duration time.Duration) AgentResult {
	ar := AgentResult{
		Role:       role,
		Duration:   duration,
		Trace:      state.PartialResult.AgentTrace,
		ExitReason: lr.ExitReason,
	}

	if lr.ReviewResult != nil {
		ar.Decision = string(lr.ReviewResult.Decision)
		ar.Confidence = lr.ReviewResult.Confidence
		ar.Violations = lr.ReviewResult.Violations

		// Try to extract reasoning from the last assistant message.
		for i := len(state.Messages) - 1; i >= 0; i-- {
			if state.Messages[i].Role == types.RoleAssistant {
				content := state.Messages[i].Content.String()
				ar.Reasoning = extractReasoning(content)
				break
			}
		}
	} else {
		// Agent failed — fail-closed.
		ar.Decision = string(types.DecisionManualReview)
		ar.Confidence = 0.0
		ar.Reasoning = fmt.Sprintf("agent failed: %s", lr.ExitReason)
	}

	return ar
}

// extractReasoning pulls the "reasoning" field from JSON output.
func extractReasoning(content string) string {
	var raw struct {
		Reasoning string `json:"reasoning"`
	}
	jsonStr := extractJSON(content)
	if jsonStr != "" {
		if err := json.Unmarshal([]byte(jsonStr), &raw); err == nil {
			return raw.Reasoning
		}
	}
	return ""
}

// extractToolsCalled returns the unique tool names called during an agent's execution.
func extractToolsCalled(state *State) []string {
	seen := map[string]bool{}
	var tools []string
	for _, t := range state.PartialResult.AgentTrace {
		if strings.HasPrefix(t, "tool_call:") {
			name := strings.TrimPrefix(t, "tool_call:")
			if !seen[name] {
				seen[name] = true
				tools = append(tools, name)
			}
		}
	}
	return tools
}

// mergeViolations collects violations from all agents, deduplicated by policy_id.
func mergeViolations(results []AgentResult) []types.PolicyViolation {
	seen := map[string]bool{}
	var merged []types.PolicyViolation
	for _, ar := range results {
		for _, v := range ar.Violations {
			if !seen[v.PolicyID] {
				seen[v.PolicyID] = true
				merged = append(merged, v)
			}
		}
	}
	return merged
}

// buildMultiAgentTrace constructs the combined trace from all agents.
func buildMultiAgentTrace(specialists []AgentResult, adj AgentResult) []string {
	var trace []string
	for _, ar := range specialists {
		trace = append(trace, fmt.Sprintf("[%s] decision=%s conf=%.2f", ar.Role, ar.Decision, ar.Confidence))
		for _, t := range ar.Trace {
			trace = append(trace, fmt.Sprintf("[%s] %s", ar.Role, t))
		}
	}
	trace = append(trace, fmt.Sprintf("[adjudicator] decision=%s conf=%.2f", adj.Decision, adj.Confidence))
	return trace
}

// noOpExecutor is used by the Adjudicator (no tools).
type noOpExecutor struct{}

func (e *noOpExecutor) Execute(_ context.Context, toolCalls []types.ToolCall) ([]types.Message, error) {
	results := make([]types.Message, len(toolCalls))
	for i, tc := range toolCalls {
		results[i] = types.Message{
			Role:       types.RoleTool,
			Content:    types.NewTextContent(`{"error":"adjudicator has no tools"}`),
			ToolCallID: tc.ID,
		}
	}
	return results, nil
}
