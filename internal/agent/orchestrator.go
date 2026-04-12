package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/memory"
	"github.com/BillBeam/adguard-agent/internal/strategy"
	"github.com/BillBeam/adguard-agent/internal/tool"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// Orchestrator implements Coordinator-driven Multi-Agent ad review.
//
// Key design decisions:
//   - Coordinator is an agentic loop with dispatch_specialist tool
//   - Coordinator dynamically decides which specialists to invoke
//   - Coordinator makes the final review decision itself (no separate Adjudicator)
//   - L3 cross-validation applied as programmatic safety net after Coordinator's decision
//   - Specialist agents reuse the same Run() function with different configs
//
// Execution flow for standard/comprehensive pipelines:
//  1. Create dispatch_specialist tool bound to review context
//  2. Run Coordinator loop (LLM calls dispatch_specialist as needed)
//  3. Coordinator outputs final ReviewResult JSON
//  4. Apply L3 cross-validation + build final result
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
	trace = append(trace, fmt.Sprintf("[%s] decision=%s conf=%.2f", adj.Role, adj.Decision, adj.Confidence))
	return trace
}

// noOpExecutor is the fallback executor for agents with no tool registry.
// Used by Appeal Agent when tool registry is not configured.
type noOpExecutor struct{}

func (e *noOpExecutor) Execute(_ context.Context, toolCalls []types.ToolCall) ([]types.Message, error) {
	results := make([]types.Message, len(toolCalls))
	for i, tc := range toolCalls {
		results[i] = types.Message{
			Role:       types.RoleTool,
			Content:    types.NewTextContent(`{"error":"no tools available"}`),
			ToolCallID: tc.ID,
		}
	}
	return results, nil
}
