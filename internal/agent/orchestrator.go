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
	"github.com/BillBeam/adguard-agent/internal/memory"
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
// ProgressFunc is called by the orchestrator when a specialist starts or completes.
// Enables real-time progress display during multi-agent review.
type ProgressFunc func(role string, phase string, detail string)

type Orchestrator struct {
	client     llm.LLMClient
	matrix     *strategy.StrategyMatrix
	registry   *tool.Registry
	router     *llm.ModelRouter  // nil = use client default
	onProgress ProgressFunc      // nil = no progress reporting
	logger     *slog.Logger
	// Tool-level hooks: injected into every specialist and adjudicator LoopConfig.
	preToolHooks  []PreToolHook
	postToolHooks []PostToolHook
	stopHooks     []StopHook
	// Agent memory: cross-review learning (nil = disabled).
	agentMemory *memory.AgentMemory
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

// WithProgress attaches a progress callback for real-time specialist status display.
func (o *Orchestrator) WithProgress(fn ProgressFunc) *Orchestrator {
	o.onProgress = fn
	return o
}

// WithModelRouter attaches model routing for per-agent model selection.
func (o *Orchestrator) WithModelRouter(router *llm.ModelRouter) *Orchestrator {
	o.router = router
	return o
}

// WithMemory attaches agent memory for cross-review learning.
func (o *Orchestrator) WithMemory(mem *memory.AgentMemory) *Orchestrator {
	o.agentMemory = mem
	return o
}

// WithHooks injects tool-level hooks into all specialist and adjudicator agents.
func (o *Orchestrator) WithHooks(pre []PreToolHook, post []PostToolHook, stop []StopHook) *Orchestrator {
	o.preToolHooks = pre
	o.postToolHooks = post
	o.stopHooks = stop
	return o
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

// RunMultiAgent executes the Coordinator-driven multi-agent review pipeline.
//
// The Coordinator is an agentic loop with a dispatch_specialist tool. It dynamically
// decides which specialists to invoke, evaluates their results, and makes the final
// review decision itself. This replaces the static fork-join + Adjudicator pattern.
//
// Flow:
//  1. Create QueryChain + DispatchSpecialist tool
//  2. Run Coordinator loop (LLM calls dispatch_specialist as needed)
//  3. Coordinator outputs final ReviewResult JSON
//  4. Apply L3 cross-validation as programmatic safety net
func (o *Orchestrator) RunMultiAgent(ctx context.Context, ad *types.AdContent, plan types.ReviewPlan) (*MultiAgentResult, error) {
	start := time.Now()
	chain := NewQueryChain()
	chainLog := NewChainLog(chain.ChainID)

	policies := o.matrix.GetApplicablePolicies(ad.Region, ad.Category)

	o.logger.Info("coordinator review started",
		slog.String("ad_id", ad.ID),
		slog.String("pipeline", plan.Pipeline),
		slog.String("chain_id", chain.ChainID[:8]),
	)

	// Create the dispatch tool bound to this review context.
	dispatchTool := NewDispatchSpecialist(
		o.client, o.matrix, o.registry, o.router, o.agentMemory,
		dispatchHooks{pre: o.preToolHooks, post: o.postToolHooks, stop: o.stopHooks},
		ad, policies, plan, chain, chainLog, o.logger,
	)

	// Attach progress callback so dispatch reports each specialist's status.
	dispatchTool.onProgress = o.onProgress

	// Build Coordinator's tool definition.
	coordToolDef := tool.ExportDefinition(dispatchTool)

	// Load coordinator memory.
	var memorySection string
	if o.agentMemory != nil {
		entries := o.agentMemory.LoadRelevant("coordinator", ad.Region, ad.Category)
		memorySection = o.agentMemory.FormatForPrompt(entries)
	}

	// Coordinator loop config: only dispatch_specialist tool.
	maxTurns := 8
	if plan.Pipeline == "comprehensive" {
		maxTurns = 12
	}

	// Wrap dispatch tool in a simple executor.
	coordExecutor := &singleToolExecutor{tool: dispatchTool}

	config := &LoopConfig{
		MaxTurns:            maxTurns,
		ConfidenceThreshold: plan.ConfidenceThreshold,
		AllowAutoReject:     plan.AllowAutoReject,
		Pipeline:            plan.Pipeline,
		Tools:               []types.ToolDefinition{coordToolDef},
		ToolExecutor:        coordExecutor,
		SystemPrompt:        BuildCoordinatorPrompt(ad, policies, plan, memorySection),
		DefaultMaxTokens:    DefaultMaxOutputTokens,
		EscalatedMaxTokens:  EscalatedMaxOutputTokens,
		MaxRecoveryAttempts: MaxRecoveryAttempts,
		PreToolHooks:        o.preToolHooks,
		PostToolHooks:       o.postToolHooks,
		StopHooks:           o.stopHooks,
		EnableStreaming:      true,
	}

	// Model routing: coordinator uses the strongest reasoning model.
	if o.router != nil {
		config.Model = o.router.RouteModel(plan.Pipeline, "adjudicator") // reuse adjudicator tier
		if fb, ok := o.router.GetFallback(config.Model); ok {
			config.FallbackModel = fb
		}
	}

	// Run Coordinator loop.
	if o.onProgress != nil {
		o.onProgress("coordinator", "start", "directing review...")
	}
	state := NewState(ad)
	state.AgentRole = "coordinator"
	state.Messages = buildInitialMessages(config.SystemPrompt)

	loopResult := Run(ctx, o.client, config, state, nil, o.logger)

	if o.onProgress != nil {
		decision := "MANUAL_REVIEW"
		conf := 0.0
		if loopResult.ReviewResult != nil {
			decision = string(loopResult.ReviewResult.Decision)
			conf = loopResult.ReviewResult.Confidence
		}
		o.onProgress("coordinator", "complete",
			fmt.Sprintf("%-15s conf=%.2f  (%s)", decision, conf, time.Since(start).Round(time.Millisecond)))
	}

	// Build final result from Coordinator's decision.
	agentResults := dispatchTool.Results()
	result := o.buildCoordinatorResult(ad, loopResult, agentResults, start)

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
			if o.onProgress != nil {
				o.onProgress(string(s.Role), "start", "analyzing...")
			}
			results[idx] = o.runSingleAgent(ctx, ad, policies, plan, s, chain.Child(), chainLog)
			ar := results[idx]
			if o.onProgress != nil {
				o.onProgress(string(s.Role), "complete",
					fmt.Sprintf("%-15s conf=%.2f  (%s)", ar.Decision, ar.Confidence, ar.Duration.Round(time.Millisecond)))
			}
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
		SystemPrompt:        o.buildSpecialistPrompt(spec.Role, ad, policies, plan),
		DefaultMaxTokens:    DefaultMaxOutputTokens,
		EscalatedMaxTokens:  EscalatedMaxOutputTokens,
		MaxRecoveryAttempts: MaxRecoveryAttempts,
	}

	// Model routing: select model based on pipeline and agent role.
	if o.router != nil {
		config.Model = o.router.RouteModel(plan.Pipeline, string(spec.Role))
		if fb, ok := o.router.GetFallback(config.Model); ok {
			config.FallbackModel = fb
		}
	}

	config.EnableStreaming = true

	// Inject tool-level hooks.
	config.PreToolHooks = o.preToolHooks
	config.PostToolHooks = o.postToolHooks
	config.StopHooks = o.stopHooks

	// Build isolated state — each agent starts with fresh messages.
	state := NewState(ad)
	state.AgentRole = string(spec.Role)
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

	// Model routing: adjudicator uses the strongest reasoning model.
	if o.router != nil {
		config.Model = o.router.RouteModel(plan.Pipeline, "adjudicator")
		if fb, ok := o.router.GetFallback(config.Model); ok {
			config.FallbackModel = fb
		}
	}

	config.EnableStreaming = true

	// Adjudicator has no tools, so PreToolHooks won't fire, but StopHooks still apply.
	config.PreToolHooks = o.preToolHooks
	config.PostToolHooks = o.postToolHooks
	config.StopHooks = o.stopHooks

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

// singleToolExecutor wraps a single Tool interface into a ToolExecutor.
// Used by the Coordinator which has only the dispatch_specialist tool.
type singleToolExecutor struct {
	tool tool.Tool
}

func (e *singleToolExecutor) Execute(ctx context.Context, toolCalls []types.ToolCall) ([]types.Message, error) {
	results := make([]types.Message, len(toolCalls))
	for i, tc := range toolCalls {
		if tc.Function.Name != e.tool.Name() {
			results[i] = types.Message{
				Role:       types.RoleTool,
				Content:    types.NewTextContent(fmt.Sprintf(`{"error":"unknown tool %q"}`, tc.Function.Name)),
				ToolCallID: tc.ID,
			}
			continue
		}
		output, err := e.tool.Execute(ctx, tc.Function.Arguments)
		if err != nil {
			results[i] = types.Message{
				Role:       types.RoleTool,
				Content:    types.NewTextContent(fmt.Sprintf(`{"error":%q}`, err.Error())),
				ToolCallID: tc.ID,
			}
		} else {
			results[i] = types.Message{
				Role:       types.RoleTool,
				Content:    types.NewTextContent(output),
				ToolCallID: tc.ID,
			}
		}
	}
	return results, nil
}

// buildCoordinatorResult constructs the final ReviewResult from the Coordinator's
// own decision + accumulated specialist results, with L3 safety net.
func (o *Orchestrator) buildCoordinatorResult(
	ad *types.AdContent,
	loopResult *LoopResult,
	agentResults []AgentResult,
	startTime time.Time,
) *types.ReviewResult {
	// Merge violations from all specialists.
	allViolations := mergeViolations(agentResults)

	// Determine risk level.
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

	// Build trace.
	trace := buildMultiAgentTrace(agentResults, AgentResult{
		Role:       "coordinator",
		Decision:   "N/A",
		Confidence: 0,
	})

	// Use Coordinator's decision if valid, otherwise programmatic aggregation.
	decision := types.DecisionManualReview
	confidence := 0.0

	if loopResult.ReviewResult != nil {
		decision = loopResult.ReviewResult.Decision
		confidence = loopResult.ReviewResult.Confidence
		// Merge coordinator's own violations if any.
		for _, v := range loopResult.ReviewResult.Violations {
			found := false
			for _, existing := range allViolations {
				if existing.PolicyID == v.PolicyID {
					found = true
					break
				}
			}
			if !found {
				allViolations = append(allViolations, v)
			}
		}
	} else {
		// Coordinator failed — use programmatic aggregation.
		o.logger.Warn("coordinator failed to produce decision, using programmatic aggregation")
		decision, confidence = aggregateDecisions(agentResults)
	}

	// L3 false-positive control: programmatic safety net.
	if len(agentResults) > 0 {
		decision, confidence = applyL3Control(agentResults, decision, confidence)
	}

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

// buildSpecialistPrompt builds a specialist agent's system prompt with optional memory injection.
func (o *Orchestrator) buildSpecialistPrompt(role AgentRole, ad *types.AdContent, policies []types.Policy, plan types.ReviewPlan) string {
	var memorySection string
	if o.agentMemory != nil {
		entries := o.agentMemory.LoadRelevant(string(role), ad.Region, ad.Category)
		memorySection = o.agentMemory.FormatForPrompt(entries)
	}
	return BuildAgentSystemPrompt(role, ad, policies, plan, memorySection)
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
