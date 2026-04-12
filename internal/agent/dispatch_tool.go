package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/memory"
	"github.com/BillBeam/adguard-agent/internal/strategy"
	"github.com/BillBeam/adguard-agent/internal/tool"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// DispatchSpecialist is a tool that the Coordinator uses to dispatch specialist
// agents for investigation. Each dispatch runs a complete specialist agentic loop
// and returns the structured result. This enables the Coordinator to dynamically
// decide which specialists to invoke and in what order.
type DispatchSpecialist struct {
	client   llm.LLMClient
	matrix   *strategy.StrategyMatrix
	registry *tool.Registry
	router   *llm.ModelRouter
	mem      *memory.AgentMemory
	hooks    dispatchHooks
	ad       *types.AdContent
	policies []types.Policy
	plan     types.ReviewPlan
	logger   *slog.Logger

	// Accumulated specialist results for L3 cross-validation.
	results    []AgentResult
	chain      *QueryChain
	log        *ChainLog
	onProgress ProgressFunc // progress callback (nil = no reporting)
}

type dispatchHooks struct {
	pre  []PreToolHook
	post []PostToolHook
	stop []StopHook
}

// NewDispatchSpecialist creates a dispatch tool bound to a specific review context.
// Created fresh for each multi-agent review.
func NewDispatchSpecialist(
	client llm.LLMClient,
	matrix *strategy.StrategyMatrix,
	registry *tool.Registry,
	router *llm.ModelRouter,
	mem *memory.AgentMemory,
	hooks dispatchHooks,
	ad *types.AdContent,
	policies []types.Policy,
	plan types.ReviewPlan,
	chain *QueryChain,
	chainLog *ChainLog,
	logger *slog.Logger,
) *DispatchSpecialist {
	return &DispatchSpecialist{
		client:   client,
		matrix:   matrix,
		registry: registry,
		router:   router,
		mem:      mem,
		hooks:    hooks,
		ad:       ad,
		policies: policies,
		plan:     plan,
		chain:    chain,
		log:      chainLog,
		logger:   logger,
	}
}

func (d *DispatchSpecialist) Name() string { return "dispatch_specialist" }

func (d *DispatchSpecialist) Description() string {
	return "Dispatch a specialist agent to investigate a specific aspect of the ad. " +
		"Available roles: content (content analysis + landing page), policy (policy compliance), " +
		"region (regional regulations + advertiser history). Returns the specialist's structured " +
		"review result including decision, confidence, violations, and reasoning."
}

func (d *DispatchSpecialist) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"role": {
				"type": "string",
				"enum": ["content", "policy", "region"],
				"description": "Specialist role to dispatch"
			},
			"focus": {
				"type": "string",
				"description": "Optional: specific investigation focus for the specialist"
			}
		},
		"required": ["role"]
	}`)
}

func (d *DispatchSpecialist) IsConcurrencySafe() bool { return false }
func (d *DispatchSpecialist) IsReadOnly() bool         { return true }
func (d *DispatchSpecialist) MaxResultSize() int       { return 0 }

func (d *DispatchSpecialist) ValidateInput(args json.RawMessage) error {
	var input struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return fmt.Errorf("parsing input: %w", err)
	}
	switch input.Role {
	case "content", "policy", "region":
		return nil
	default:
		return fmt.Errorf("invalid role %q: must be content, policy, or region", input.Role)
	}
}

// Execute dispatches a specialist agent and returns its structured result.
func (d *DispatchSpecialist) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input struct {
		Role  string `json:"role"`
		Focus string `json:"focus"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	role := AgentRole(input.Role)
	spec := d.specForRole(role)
	start := time.Now()
	childChain := d.chain.Child()

	// Build specialist config (reuses existing specialist infrastructure).
	subRegistry := d.registry.Sub(spec.Tools...)
	subExecutor := tool.NewExecutor(subRegistry, d.logger)

	// Load relevant memories for this specialist.
	var memorySection string
	if d.mem != nil {
		entries := d.mem.LoadRelevant(string(role), d.ad.Region, d.ad.Category)
		memorySection = d.mem.FormatForPrompt(entries)
	}

	config := &LoopConfig{
		MaxTurns:            spec.MaxTurns,
		ConfidenceThreshold: d.plan.ConfidenceThreshold,
		AllowAutoReject:     d.plan.AllowAutoReject,
		RequireVerification: false,
		Pipeline:            d.plan.Pipeline,
		Tools:               subRegistry.ExportDefinitions(),
		ToolExecutor:        subExecutor,
		SystemPrompt:        BuildAgentSystemPrompt(role, d.ad, d.policies, d.plan, memorySection),
		DefaultMaxTokens:    DefaultMaxOutputTokens,
		EscalatedMaxTokens:  EscalatedMaxOutputTokens,
		MaxRecoveryAttempts: MaxRecoveryAttempts,
		PreToolHooks:        d.hooks.pre,
		PostToolHooks:       d.hooks.post,
		StopHooks:           d.hooks.stop,
		EnableStreaming:      true,
	}

	// Model routing.
	if d.router != nil {
		config.Model = d.router.RouteModel(d.plan.Pipeline, string(role))
		if fb, ok := d.router.GetFallback(config.Model); ok {
			config.FallbackModel = fb
		}
	}

	// Execute specialist.
	state := NewState(d.ad)
	state.AgentRole = string(role)
	state.Messages = buildInitialMessages(config.SystemPrompt)

	if d.onProgress != nil {
		d.onProgress(string(role), "start", "analyzing...")
	}

	d.logger.Info("specialist dispatched",
		slog.String("role", string(role)),
		slog.String("ad_id", d.ad.ID),
	)

	loopResult := Run(ctx, d.client, config, state, nil, d.logger)
	duration := time.Since(start)
	ar := parseAgentResult(role, loopResult, state, duration)

	// Record in chain log.
	d.log.Add(ChainEntry{
		ChainID:    childChain.ChainID,
		Depth:      childChain.Depth,
		AgentRole:  string(role),
		Decision:   ar.Decision,
		Confidence: ar.Confidence,
		ToolsCalled: extractToolsCalled(state),
		Duration:   duration,
		Trace:      ar.Trace,
	})

	// Accumulate for L3 cross-validation.
	d.results = append(d.results, ar)

	if d.onProgress != nil {
		d.onProgress(string(role), "complete",
			fmt.Sprintf("%-15s conf=%.2f  (%s)", ar.Decision, ar.Confidence, duration.Round(time.Millisecond)))
	}

	d.logger.Info("specialist completed",
		slog.String("role", string(role)),
		slog.String("decision", ar.Decision),
		slog.Float64("confidence", ar.Confidence),
		slog.Duration("duration", duration),
	)

	// Return structured result JSON for the Coordinator to read.
	result, _ := json.Marshal(map[string]any{
		"role":        ar.Role,
		"decision":    ar.Decision,
		"confidence":  ar.Confidence,
		"violations":  ar.Violations,
		"reasoning":   ar.Reasoning,
		"exit_reason": ar.ExitReason,
		"duration":    duration.String(),
	})
	return string(result), nil
}

func (d *DispatchSpecialist) specForRole(role AgentRole) AgentSpec {
	specs := SpecialistAgentSpecs(d.plan.Pipeline)
	for _, s := range specs {
		if s.Role == role {
			return s
		}
	}
	// Fallback: content agent defaults.
	return AgentSpec{Role: role, Tools: []string{"analyze_content", "check_landing_page"}, MaxTurns: 4}
}

// Results returns all accumulated specialist results for L3 cross-validation.
func (d *DispatchSpecialist) Results() []AgentResult {
	return d.results
}
