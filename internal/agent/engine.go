package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"errors"

	"github.com/BillBeam/adguard-agent/internal/compact"
	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/memory"
	"github.com/BillBeam/adguard-agent/internal/store"
	"github.com/BillBeam/adguard-agent/internal/tool"
	"github.com/BillBeam/adguard-agent/internal/strategy"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// PostReviewHook is called after each review completes.
// Implementations can record the result for history/metrics.
type PostReviewHook interface {
	PostReview(result types.ReviewResult, advertiserID, region, category, pipeline string)
}


// ReviewEngine manages ad review sessions.
// It bridges the strategy matrix (what policies apply) with the agentic loop
// (how to analyze and decide).
type ReviewEngine struct {
	client        llm.LLMClient
	matrix        *strategy.StrategyMatrix
	logger        *slog.Logger
	tools         []types.ToolDefinition
	executor      ToolExecutor
	postReviewHook PostReviewHook // optional, may be nil

	// Phase 3: Context Management + Verification.
	contextManager *compact.ContextManager // nil = disabled
	tokenBudget    *compact.TokenBudget    // nil = disabled
	reviewStore    *store.ReviewStore      // nil = disabled
	verifier       *store.Verifier         // nil = disabled

	// Phase 4: Multi-Agent orchestration.
	orchestrator *Orchestrator // nil = single-agent only (fast pipeline)

	// Phase 5: Governance — appeal, training, version, reputation.
	trainingPool   *store.TrainingPool       // nil = no training data collection
	appealStore    *store.AppealStore        // nil = no appeal support
	reputationMgr  *store.ReputationManager  // nil = no reputation tracking
	versionManager *strategy.VersionManager  // nil = no version routing

	// Model routing: per-pipeline and per-role model selection.
	router *llm.ModelRouter // nil = use LLM client default model

	// Tool-level hooks: injected into every LoopConfig for audit trail and circuit-breaker protection.
	preToolHooks  []PreToolHook
	postToolHooks []PostToolHook
	stopHooks     []StopHook

	// Agent memory: cross-review learning (nil = disabled).
	agentMemory *memory.AgentMemory

	// Tool registry: used to create sub-registries for Appeal Agent.
	toolRegistry *tool.Registry
}

// NewReviewEngine creates a review engine with the given tools.
// Pass mock.ToolDefinitions()/mock.NewToolExecutor() for Phase 1,
// real tools for Phase 2+.
// postReviewHook is optional (nil allowed) — when set, called after each review
// to record results (e.g., HistoryLookup in tool.Executor).
func NewReviewEngine(
	client llm.LLMClient,
	matrix *strategy.StrategyMatrix,
	tools []types.ToolDefinition,
	executor ToolExecutor,
	logger *slog.Logger,
	postReviewHook PostReviewHook,
) *ReviewEngine {
	return &ReviewEngine{
		client:         client,
		matrix:         matrix,
		logger:         logger,
		tools:          tools,
		executor:       executor,
		postReviewHook: postReviewHook,
	}
}

// WithPhase3 attaches Phase 3 components to the engine.
// Builder pattern — does not change NewReviewEngine signature (backward compatible).
func (e *ReviewEngine) WithPhase3(
	cm *compact.ContextManager,
	tb *compact.TokenBudget,
	rs *store.ReviewStore,
	v *store.Verifier,
) *ReviewEngine {
	e.contextManager = cm
	e.tokenBudget = tb
	e.reviewStore = rs
	e.verifier = v
	return e
}

// WithOrchestrator attaches the Multi-Agent orchestrator.
// When set, standard/comprehensive pipelines use multi-agent review.
// Fast pipeline always uses single-agent regardless.
func (e *ReviewEngine) WithOrchestrator(o *Orchestrator) *ReviewEngine {
	e.orchestrator = o
	return e
}

// WithPhase5 attaches governance components: training pool, appeal, reputation, version management.
func (e *ReviewEngine) WithPhase5(
	tp *store.TrainingPool,
	as *store.AppealStore,
	rm *store.ReputationManager,
	vm *strategy.VersionManager,
) *ReviewEngine {
	e.trainingPool = tp
	e.appealStore = as
	e.reputationMgr = rm
	e.versionManager = vm
	return e
}

// WithModelRouter attaches model routing for per-pipeline/per-role model selection.
// When set, each review uses the model determined by the router based on the
// pipeline (fast/standard/comprehensive) and agent role (content/policy/region/coordinator).
func (e *ReviewEngine) WithModelRouter(router *llm.ModelRouter) *ReviewEngine {
	e.router = router
	return e
}

// WithToolRegistry attaches the tool registry for creating sub-registries (e.g., Appeal Agent).
func (e *ReviewEngine) WithToolRegistry(reg *tool.Registry) *ReviewEngine {
	e.toolRegistry = reg
	return e
}

// WithMemory attaches agent memory for cross-review learning.
func (e *ReviewEngine) WithMemory(mem *memory.AgentMemory) *ReviewEngine {
	e.agentMemory = mem
	return e
}

// WithHooks injects tool-level hooks. Automatically applied when building each LoopConfig.
func (e *ReviewEngine) WithHooks(pre []PreToolHook, post []PostToolHook, stop []StopHook) *ReviewEngine {
	e.preToolHooks = pre
	e.postToolHooks = post
	e.stopHooks = stop
	return e
}

// ProcessAppeal runs the Appeal Agent to re-review a REJECTED ad.
// The Appeal Agent sees: ad content + original decision + violations + appeal reason.
// It does NOT see agent_trace (independence from original review).
// fail-closed: Agent failure → UPHELD (maintain original REJECTED).
// Returns the outcome, LoopResult (for trace/reasoning inspection), and error.
func (e *ReviewEngine) ProcessAppeal(
	ctx context.Context,
	ad *types.AdContent,
	record *store.ReviewRecord,
	appeal *store.Appeal,
) (store.AppealOutcome, *LoopResult, error) {
	if e.appealStore == nil {
		return store.AppealUpheld, nil, fmt.Errorf("appeal store not configured")
	}

	// Transition to REVIEWING.
	e.appealStore.SetReviewing(appeal.AppealID)

	// Build Appeal Agent config with investigation tools.
	// Appeal Agent can independently re-verify facts (landing page, policies, history)
	// but does NOT see the original agent_trace (independence preserved).
	var appealTools []types.ToolDefinition
	var appealExecutor ToolExecutor = &noOpExecutor{}
	if e.toolRegistry != nil {
		appealReg := e.toolRegistry.Sub("check_landing_page", "query_policy_kb", "lookup_history")
		appealTools = appealReg.ExportDefinitions()
		appealExecutor = tool.NewExecutor(appealReg, e.logger)
	}

	config := &LoopConfig{
		MaxTurns:            5, // more turns for tool-based investigation
		ConfidenceThreshold: 0.7,
		AllowAutoReject:     false,
		Pipeline:            "appeal",
		Tools:               appealTools,
		ToolExecutor:        appealExecutor,
		SystemPrompt:        BuildAppealSystemPrompt(ad, record, appeal.Reason),
		DefaultMaxTokens:    DefaultMaxOutputTokens,
		EscalatedMaxTokens:  EscalatedMaxOutputTokens,
		MaxRecoveryAttempts: MaxRecoveryAttempts,
		EnableStreaming:     true,
	}

	state := NewState(ad)
	state.AgentRole = "appeal"
	state.Messages = []types.Message{
		{Role: types.RoleSystem, Content: types.NewTextContent(config.SystemPrompt)},
		{Role: types.RoleUser, Content: types.NewTextContent(
			"Based on the ad content, original review decision, and the advertiser's appeal reason, " +
				"determine whether the REJECTED decision should be upheld or overturned. Output as JSON.")},
	}

	e.logger.Info("appeal review started",
		slog.String("ad_id", ad.ID),
		slog.String("appeal_id", appeal.AppealID),
	)

	loopResult := Run(ctx, e.client, config, state, nil, e.logger)

	// Parse Appeal Agent's decision.
	outcome := store.AppealUpheld // fail-closed default
	reasoning := "appeal agent failed to produce decision"

	if loopResult.ReviewResult != nil {
		switch loopResult.ReviewResult.Decision {
		case types.DecisionPassed:
			outcome = store.AppealOverturned
			reasoning = "appeal agent recommends overturning the rejection"
		case types.DecisionManualReview:
			outcome = store.AppealPartial
			reasoning = "appeal agent recommends partial review"
		default:
			outcome = store.AppealUpheld
			reasoning = "appeal agent confirms the rejection"
		}
		// Extract reasoning from LLM output if available.
		for i := len(state.Messages) - 1; i >= 0; i-- {
			if state.Messages[i].Role == types.RoleAssistant {
				if r := extractReasoning(state.Messages[i].Content.String()); r != "" {
					reasoning = r
				}
				break
			}
		}
	}

	// Resolve the appeal.
	e.appealStore.Resolve(appeal.AppealID, outcome, reasoning)

	// OVERTURNED: update ReviewStore + TrainingPool + HistoryLookup.
	if outcome == store.AppealOverturned {
		if e.reviewStore != nil {
			// Update the review record decision to PASSED.
			if rec, ok := e.reviewStore.Get(ad.ID); ok {
				rec.Decision = types.DecisionPassed
			}
		}
		if e.trainingPool != nil {
			e.trainingPool.Add(&store.TrainingRecord{
				AdID:             ad.ID,
				AdContent:        ad,
				OriginalDecision: types.DecisionRejected,
				FinalDecision:    types.DecisionPassed,
				Source:           store.SourceAppealOverturn,
				Region:           ad.Region,
				Category:         ad.Category,
			})
		}
	}

	e.logger.Info("appeal resolved",
		slog.String("ad_id", ad.ID),
		slog.String("outcome", string(outcome)),
	)

	return outcome, loopResult, nil
}

// Review executes a complete review for a single ad.
// Returns the LoopResult containing the review decision, or an error if
// the review could not be initiated (config/data errors).
func (e *ReviewEngine) Review(ctx context.Context, ad *types.AdContent) (*LoopResult, error) {
	// 1. Query strategy matrix for review plan and applicable policies.
	if ad == nil {
		return nil, fmt.Errorf("ad content is nil")
	}
	plan := e.matrix.GetReviewPlan(ad.Region, ad.Category)
	policies := e.matrix.GetApplicablePolicies(ad.Region, ad.Category)

	e.logger.Info("review started",
		slog.String("ad_id", ad.ID),
		slog.String("region", ad.Region),
		slog.String("category", ad.Category),
		slog.String("pipeline", plan.Pipeline),
		slog.Int("max_turns", plan.MaxTurns),
		slog.Int("policies", len(policies)),
	)

	// 2. Route by pipeline: Multi-Agent (standard/comprehensive) or single Agent (fast).
	if e.orchestrator != nil && plan.Pipeline != "fast" && len(plan.RequiredAgents) > 1 {
		return e.reviewMultiAgent(ctx, ad, plan)
	}

	// Single-agent path (fast pipeline or no orchestrator).
	var memorySection string
	if e.agentMemory != nil {
		entries := e.agentMemory.LoadRelevant("single", ad.Region, ad.Category)
		memorySection = e.agentMemory.FormatForPrompt(entries)
	}
	config := NewLoopConfig(plan, ad, policies, e.tools, e.executor, memorySection)
	if e.contextManager != nil || e.tokenBudget != nil {
		config.WithContextManagement(e.contextManager, e.tokenBudget)
	}

	// Model routing: select model based on pipeline.
	if e.router != nil {
		config.Model = e.router.RouteModel(plan.Pipeline, "")
		if fb, ok := e.router.GetFallback(config.Model); ok {
			config.FallbackModel = fb
		}
	}

	// Streaming: dispatch tools during LLM response for lower latency.
	// callAPIStreaming has built-in non-streaming fallback on errors.
	config.EnableStreaming = true

	// Inject tool-level hooks (audit trail, circuit-breaker protection, etc.).
	config.PreToolHooks = e.preToolHooks
	config.PostToolHooks = e.postToolHooks
	config.StopHooks = e.stopHooks

	// 3. Initialize state.
	state := NewState(ad)
	state.AgentRole = "single"
	state.Messages = buildInitialMessages(config.SystemPrompt)

	// 4. Create events channel + consumer goroutine.
	events := make(chan StreamEvent, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			// Streaming events at INFO level for demo observability.
			if ev.Type == EventStreamStarted || ev.Type == EventStreamToolDispatched ||
				ev.Type == EventStreamToolCompleted || ev.Type == EventStreamFallback {
				e.logger.Info("stream",
					slog.String("event", string(ev.Type)),
					slog.String("detail", ev.Detail),
				)
			} else {
				e.logger.Debug("loop event",
					slog.String("type", string(ev.Type)),
					slog.String("state", string(ev.State)),
					slog.Int("turn", ev.TurnCount),
					slog.String("detail", ev.Detail),
				)
			}
		}
	}()

	// 5. Execute agentic loop.
	result := Run(ctx, e.client, config, state, events, e.logger)

	// 529 fallback: if consecutive overload errors triggered model downgrade, retry with fallback.
	if result.Error != nil {
		var fallbackErr *llm.FallbackTriggeredError
		if errors.As(result.Error, &fallbackErr) && e.router != nil {
			if fb, ok := e.router.GetFallback(config.Model); ok {
				e.logger.Warn("529 model fallback triggered",
					slog.String("original", config.Model),
					slog.String("fallback", fb),
					slog.Int("consecutive_529", fallbackErr.Consecutive),
				)
				config.Model = fb
				state = NewState(ad)
				state.Messages = buildInitialMessages(config.SystemPrompt)
				result = Run(ctx, e.client, config, state, events, e.logger)
			}
		}
	}

	close(events)
	<-done // wait for event consumer to finish

	// 6. Log result.
	duration := time.Since(state.StartedAt)
	e.logger.Info("review completed",
		slog.String("ad_id", ad.ID),
		slog.String("exit_reason", string(result.ExitReason)),
		slog.Int("turns", state.TurnCount),
		slog.Duration("duration", duration),
	)

	if result.ReviewResult != nil {
		e.logger.Info("review decision",
			slog.String("ad_id", ad.ID),
			slog.String("decision", string(result.ReviewResult.Decision)),
			slog.Float64("confidence", result.ReviewResult.Confidence),
			slog.Int("violations", len(result.ReviewResult.Violations)),
		)
		// Record result for history/metrics via hook chain.
		if e.postReviewHook != nil {
			e.postReviewHook.PostReview(*result.ReviewResult, ad.AdvertiserID, ad.Region, ad.Category, config.Pipeline)
		}

		// Phase 5: stamp version ID on the review record.
		if e.versionManager != nil && e.reviewStore != nil {
			versionID := e.versionManager.RouteTraffic(ad.ID)
			e.reviewStore.SetVersionID(ad.ID, versionID)
		}

		// Phase 3: Verification — independent LLM-as-Judge re-check for REJECTED.
		// Triggered after hook chain (which writes to ReviewStore).
		if result.ReviewResult.Decision == types.DecisionRejected &&
			config.RequireVerification &&
			e.verifier != nil {

			vr, verErr := e.verifier.Verify(ctx, result.ReviewResult.AdID, ad)
			if verErr != nil {
				e.logger.Warn("verification error, defaulting to MANUAL_REVIEW",
					slog.String("ad_id", ad.ID), slog.String("error", verErr.Error()))
				result.ReviewResult.Decision = types.DecisionManualReview
				state.AppendTrace("verification_error: REJECTED → MANUAL_REVIEW")
			} else if !vr.Agree {
				result.ReviewResult.Decision = types.DecisionManualReview
				state.AppendTrace(fmt.Sprintf("verification_override: REJECTED → MANUAL_REVIEW (%s)", vr.Reasoning))
			} else {
				state.AppendTrace("verification_confirmed: REJECTED maintained")
			}
		}
	}

	e.logger.Debug("transition log",
		slog.String("ad_id", ad.ID),
		slog.String("transitions", state.FormatTransitionLog()),
	)

	return result, nil
}

// reviewMultiAgent delegates to the Orchestrator for standard/comprehensive pipelines.
func (e *ReviewEngine) reviewMultiAgent(ctx context.Context, ad *types.AdContent, plan types.ReviewPlan) (*LoopResult, error) {
	start := time.Now()

	multiResult, err := e.orchestrator.RunMultiAgent(ctx, ad, plan)
	if err != nil {
		e.logger.Error("multi-agent review failed", slog.String("ad_id", ad.ID), slog.String("error", err.Error()))
		return nil, err
	}

	result := multiResult.ReviewResult
	result.ReviewDuration = time.Since(start)

	// Build a LoopResult wrapper for compatibility with existing PostReviewHook / Verification.
	state := NewState(ad)
	state.PartialResult.AgentTrace = result.AgentTrace
	lr := &LoopResult{
		ExitReason:       ExitCompleted,
		ReviewResult:     result,
		MultiAgentDetail: multiResult,
		State:            state,
	}

	e.logger.Info("multi-agent review completed",
		slog.String("ad_id", ad.ID),
		slog.String("decision", string(result.Decision)),
		slog.Float64("confidence", result.Confidence),
		slog.Int("agents", len(multiResult.AgentResults)),
	)

	// Run PostReviewHook chain (same as single-agent path).
	if e.postReviewHook != nil {
		e.postReviewHook.PostReview(*result, ad.AdvertiserID, ad.Region, ad.Category, plan.Pipeline)
	}

	// Phase 5: stamp version ID (same as single-agent path).
	if e.versionManager != nil && e.reviewStore != nil {
		versionID := e.versionManager.RouteTraffic(ad.ID)
		e.reviewStore.SetVersionID(ad.ID, versionID)
	}

	// Phase 3: Verification (for comprehensive pipeline).
	if result.Decision == types.DecisionRejected && plan.RequireVerification && e.verifier != nil {
		vr, verErr := e.verifier.Verify(ctx, result.AdID, ad)
		if verErr != nil || !vr.Agree {
			result.Decision = types.DecisionManualReview
			state.AppendTrace("verification_override: REJECTED → MANUAL_REVIEW")
		} else {
			state.AppendTrace("verification_confirmed: REJECTED maintained")
		}
	}

	// Log chain.
	if multiResult.ChainLog != nil {
		e.logger.Debug("query chain", slog.String("chain", multiResult.ChainLog.Format()))
	}

	return lr, nil
}

// ReviewBatch reviews multiple ads sequentially, sharing Context Management
// and Token Budget across reviews. Each review resets per-review budget but
// accumulates batch budget.
func (e *ReviewEngine) ReviewBatch(ctx context.Context, ads []*types.AdContent) ([]*LoopResult, error) {
	results := make([]*LoopResult, 0, len(ads))

	for _, ad := range ads {
		// Reset per-review budget (batch budget continues accumulating).
		if e.tokenBudget != nil {
			e.tokenBudget.ResetForReview()
		}

		result, err := e.Review(ctx, ad)
		if err != nil {
			e.logger.Error("batch review failed", slog.String("ad_id", ad.ID), slog.String("error", err.Error()))
			continue
		}
		results = append(results, result)

		// Check context cancellation between reviews.
		select {
		case <-ctx.Done():
			e.logger.Warn("batch review cancelled", slog.Int("completed", len(results)), slog.Int("total", len(ads)))
			return results, ctx.Err()
		default:
		}
	}
	return results, nil
}

// buildInitialMessages creates the initial message sequence for the review loop.
func buildInitialMessages(systemPrompt string) []types.Message {
	return []types.Message{
		{
			Role:    types.RoleSystem,
			Content: types.NewTextContent(systemPrompt),
		},
		{
			Role: types.RoleUser,
			Content: types.NewTextContent(
				"Please review this advertisement for policy compliance. " +
					"Use the available tools to analyze the content, check policies, " +
					"and verify the landing page. Then provide your final review decision as JSON."),
		},
	}
}
