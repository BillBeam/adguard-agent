package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"sync"

	"time"

	"github.com/BillBeam/adguard-agent/internal/agent"
	"github.com/BillBeam/adguard-agent/internal/agent/mock"
	"github.com/BillBeam/adguard-agent/internal/compact"
	"github.com/BillBeam/adguard-agent/internal/config"
	"github.com/BillBeam/adguard-agent/internal/llm"
	"github.com/BillBeam/adguard-agent/internal/recheck"
	"github.com/BillBeam/adguard-agent/internal/shutdown"
	"github.com/BillBeam/adguard-agent/internal/store"
	"github.com/BillBeam/adguard-agent/internal/strategy"
	"github.com/BillBeam/adguard-agent/internal/tool"
	"github.com/BillBeam/adguard-agent/internal/types"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: config.ParseLogLevel(cfg.Logging.Level),
	}))
	slog.SetDefault(logger)

	matrix, err := strategy.NewStrategyMatrix(
		filepath.Join(cfg.Data.Dir, cfg.Data.PolicyKBFile),
		filepath.Join(cfg.Data.Dir, cfg.Data.RegionRulesFile),
		filepath.Join(cfg.Data.Dir, cfg.Data.CategoryRiskFile),
		logger,
	)
	if err != nil {
		logger.Error("failed to load strategy matrix", "error", err)
		os.Exit(1)
	}

	// Graceful shutdown: wait for in-flight reviews, then flush stores.
	var reviewWg sync.WaitGroup
	shutdown.Setup(&reviewWg, logger)

	logger.Info("AdGuard Agent started",
		"provider", cfg.LLM.Provider,
		"model", cfg.LLM.Model,
		"policies", len(matrix.Policies),
		"regions", len(matrix.RegionRules.Rules),
	)

	if cfg.LLM.APIKey != "" {
		runWithRealLLM(cfg, matrix, logger, &reviewWg)
	} else {
		runWithMockLLM(matrix, logger, cfg, &reviewWg)
	}
}

func runWithRealLLM(cfg *config.Config, matrix *strategy.StrategyMatrix, logger *slog.Logger, reviewWg *sync.WaitGroup) {
	client, err := llm.NewClient(llm.ProviderConfig{
		Name: cfg.LLM.Provider, BaseURL: cfg.LLM.BaseURL,
		APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model,
		MaxRetries: cfg.LLM.MaxRetries, Timeout: cfg.LLM.Timeout,
	}, logger)
	if err != nil {
		logger.Error("failed to create LLM client", "error", err)
		os.Exit(1)
	}

	samples, err := loadSamples(filepath.Join(cfg.Data.Dir, cfg.Data.SamplesFile))
	if err != nil || len(samples) == 0 {
		logger.Error("failed to load samples", "error", err)
		os.Exit(1)
	}

	engine, stores := buildEngine(client, matrix, logger, cfg.Data.Dir)

	limit := 3
	if len(samples) < limit {
		limit = len(samples)
	}

	printBanner()
	fmt.Print(stores.router.FormatRoutingTable())
	fmt.Printf("\n=== Review: %d ads ===\n", limit)
	for i := 0; i < limit; i++ {
		ad := &samples[i].AdContent
		stores.budget.ResetForReview()
		// Print ad header BEFORE review starts — progress lines appear during review.
		plan := matrix.GetReviewPlan(ad.Region, ad.Category)
		mode := "single-agent"
		if plan.Pipeline != "fast" {
			mode = "multi-agent"
		}
		fmt.Printf("\n--- %s (%s/%s) [%s] ---\n", ad.ID, ad.Region, ad.Category, mode)

		reviewWg.Add(1)
		result, reviewErr := engine.Review(context.Background(), ad)
		reviewWg.Done()
		if reviewErr != nil {
			fmt.Printf("  ERROR: %s\n", reviewErr)
			continue
		}
		printReviewResult(ad, result, samples[i].ExpectedResult, stores)
	}

	demoAppeal(context.Background(), engine, stores, samples, logger)
	demoVersionManager(stores.versionMgr, logger)
	printFeatureShowcase(stores, client)
}

func runWithMockLLM(matrix *strategy.StrategyMatrix, logger *slog.Logger, cfg *config.Config, reviewWg *sync.WaitGroup) {
	logger.Info("no LLM_API_KEY set, running all samples with mock LLM + mock tools")

	samples, err := loadSamples(filepath.Join(cfg.Data.Dir, cfg.Data.SamplesFile))
	if err != nil {
		logger.Error("failed to load samples", "error", err)
		os.Exit(1)
	}

	// Shared Phase 5 components.
	reviewStore := store.NewReviewStore(logger, "")
	reputationMgr := store.NewReputationManager(logger)
	appealStore := store.NewAppealStore(logger, reputationMgr, "")
	trainingPool := store.NewTrainingPool(logger, "")
	versionMgr := strategy.NewVersionManager(logger)
	recheckScheduler := recheck.NewRecheckScheduler(logger, "")
	recheckHook := recheck.NewRecheckHook(recheckScheduler, matrix.GetRiskLevel, 24*time.Hour)
	auditHook := agent.NewAuditHook(logger)
	cbHook := agent.NewCircuitBreakerHook(10, logger)
	validationHook := agent.NewResultValidationHook(logger)
	finalAuditHook := agent.NewFinalAuditHook(logger)
	preHooks := []agent.PreToolHook{auditHook, cbHook}
	postHooks := []agent.PostToolHook{auditHook, cbHook}
	stopHooks := []agent.StopHook{validationHook, finalAuditHook}

	// Setup version: v1.0 active.
	versionMgr.Create("v1.0")
	versionMgr.Deploy("v1.0", 0)
	versionMgr.Promote("v1.0")

	// Model routing (same config as real LLM mode).
	router := llm.NewModelRouter(llm.DefaultRoutingConfig(), logger)
	printBanner()
	fmt.Print(router.FormatRoutingTable())
	fmt.Printf("\n=== Review: %d ads (mock mode) ===\n", len(samples))

	for i := range samples {
		ad := &samples[i].AdContent
		client := mock.NewLLMClient()
		reg := tool.NewReviewRegistry(client, matrix, reviewStore, logger)
		executor := tool.NewExecutor(reg, logger)

		hookChain := agent.NewHookChain(logger).Add(executor).Add(reviewStore).Add(trainingPool).Add(recheckHook)
		orchestrator := agent.NewOrchestrator(client, matrix, reg, logger)
		orchestrator.WithModelRouter(router)
		orchestrator.WithHooks(preHooks, postHooks, stopHooks)
		orchestrator.WithProgress(func(role, phase, detail string) {
			if phase == "start" {
				fmt.Printf("  ├─ %-14s %s\n", role+":", detail)
			} else {
				fmt.Printf("  ├─ %-14s %s\n", role+":", detail)
			}
		})

		engine := agent.NewReviewEngine(client, matrix, reg.ExportDefinitions(), executor, logger, hookChain)
		engine.WithOrchestrator(orchestrator)
		engine.WithModelRouter(router)
		engine.WithHooks(preHooks, postHooks, stopHooks)
		engine.WithPhase3(nil, nil, reviewStore, nil)
		engine.WithPhase5(trainingPool, appealStore, reputationMgr, versionMgr)

		// Print ad header before review (progress lines appear during review).
		plan := matrix.GetReviewPlan(ad.Region, ad.Category)
		mode := "single-agent"
		if plan.Pipeline != "fast" {
			mode = "multi-agent"
		}
		fmt.Printf("\n--- %s (%s/%s) [%s] ---\n", ad.ID, ad.Region, ad.Category, mode)

		reviewWg.Add(1)
		result, reviewErr := engine.Review(context.Background(), ad)
		reviewWg.Done()
		if reviewErr != nil {
			fmt.Printf("  ERROR: %s\n", reviewErr)
			continue
		}
		if result.ReviewResult == nil {
			continue
		}

		mockStores := &engineStores{
			reviewStore: reviewStore, recheckScheduler: recheckScheduler,
			versionMgr: versionMgr,
		}
		printReviewResult(ad, result, samples[i].ExpectedResult, mockStores)
	}

	// Simulate verification.
	simulateVerification(reviewStore, trainingPool, matrix, logger)

	// Appeal demo.
	rejected := reviewStore.QueryByDecision(types.DecisionRejected)
	appealCount := 0
	for _, rec := range rejected {
		if appealCount >= 2 {
			break
		}
		appeal, err := appealStore.Submit(rec.AdID, rec.AdvertiserID, "We believe this ad is compliant")
		if err != nil {
			continue
		}
		// Simulate appeal outcome: first UPHELD, second OVERTURNED.
		if appealCount == 0 {
			appealStore.Resolve(appeal.AppealID, store.AppealUpheld, "simulated: violations confirmed")
		} else {
			appealStore.Resolve(appeal.AppealID, store.AppealOverturned, "simulated: ad is actually compliant")
			trainingPool.Add(&store.TrainingRecord{
				AdID: rec.AdID, Source: store.SourceAppealOverturn,
				OriginalDecision: types.DecisionRejected, FinalDecision: types.DecisionPassed,
				Region: rec.Region, Category: rec.Category,
			})
		}
		appealCount++
	}

	// Version demo.
	versionMgr.Create("v2.0")
	versionMgr.Deploy("v2.0", 10)

	// Feature showcase (same format as real LLM mode).
	mockStores := &engineStores{
		reviewStore: reviewStore, versionMgr: versionMgr,
		recheckScheduler: recheckScheduler, auditHook: auditHook,
		trainingPool: trainingPool, appealStore: appealStore,
	}
	printFeatureShowcase(mockStores, nil)
}

// --- Shared helpers ---

type engineStores struct {
	reviewStore      *store.ReviewStore
	trainingPool     *store.TrainingPool
	appealStore      *store.AppealStore
	reputationMgr    *store.ReputationManager
	versionMgr       *strategy.VersionManager
	recheckScheduler *recheck.RecheckScheduler
	budget           *compact.TokenBudget
	auditHook     *agent.AuditHook
	router        *llm.ModelRouter
}

func buildEngine(client llm.LLMClient, matrix *strategy.StrategyMatrix, logger *slog.Logger, dataDir string) (*agent.ReviewEngine, *engineStores) {
	// JSONL persistence: each store gets its own file in the data directory.
	reviewStore := store.NewReviewStore(logger, filepath.Join(dataDir, "reviews.jsonl"))
	reputationMgr := store.NewReputationManager(logger)
	appealStore := store.NewAppealStore(logger, reputationMgr, filepath.Join(dataDir, "appeals.jsonl"))
	trainingPool := store.NewTrainingPool(logger, filepath.Join(dataDir, "training.jsonl"))

	// Register JSONL flush for graceful shutdown.
	shutdown.RegisterCleanup(func() { reviewStore.Flush() })
	shutdown.RegisterCleanup(func() { appealStore.Flush() })
	shutdown.RegisterCleanup(func() { trainingPool.Flush() })
	versionMgr := strategy.NewVersionManager(logger)
	verifier := store.NewVerifier(client, reviewStore, logger).WithTrainingPool(trainingPool)
	auditHook := agent.NewAuditHook(logger)
	cbHook := agent.NewCircuitBreakerHook(10, logger) // High threshold so demo doesn't accidentally trip
	validationHook := agent.NewResultValidationHook(logger)
	finalAuditHook := agent.NewFinalAuditHook(logger)

	reg := tool.NewReviewRegistry(client, matrix, reviewStore, logger)
	resultBudget := tool.NewResultBudget(filepath.Join(dataDir, "tool-results"), logger)
	executor := tool.NewExecutor(reg, logger).WithBudget(resultBudget)

	// Scheduled recheck: re-review high-risk PASSED ads after 24h.
	recheckScheduler := recheck.NewRecheckScheduler(logger, filepath.Join(dataDir, "rechecks.jsonl"))
	shutdown.RegisterCleanup(func() { recheckScheduler.Flush() })
	recheckHook := recheck.NewRecheckHook(recheckScheduler, matrix.GetRiskLevel, 24*time.Hour)

	hookChain := agent.NewHookChain(logger).Add(executor).Add(reviewStore).Add(trainingPool).Add(recheckHook)

	// Model routing: per-pipeline and per-role model selection.
	router := llm.NewModelRouter(llm.DefaultRoutingConfig(), logger)
	orchestrator := agent.NewOrchestrator(client, matrix, reg, logger)
	orchestrator.WithModelRouter(router)
	orchestrator.WithProgress(func(role, phase, detail string) {
		if phase == "start" {
			fmt.Printf("  ├─ %-14s %s\n", role+":", detail)
		} else {
			fmt.Printf("  ├─ %-14s %s\n", role+":", detail)
		}
	})

	ctxMgr := compact.NewContextManager(compact.DefaultCompactConfig(), client, logger)
	budget := compact.NewTokenBudget(compact.DefaultBudgetConfig())

	// Setup version: v1.0 active.
	versionMgr.Create("v1.0")
	versionMgr.Deploy("v1.0", 0)
	versionMgr.Promote("v1.0")

	// Tool-level hooks: audit trail + circuit-breaker protection.
	preHooks := []agent.PreToolHook{auditHook, cbHook}
	postHooks := []agent.PostToolHook{auditHook, cbHook}
	stopHooks := []agent.StopHook{validationHook, finalAuditHook}

	orchestrator.WithHooks(preHooks, postHooks, stopHooks)

	engine := agent.NewReviewEngine(client, matrix, reg.ExportDefinitions(), executor, logger, hookChain)
	engine.WithPhase3(ctxMgr, budget, reviewStore, verifier)
	engine.WithOrchestrator(orchestrator)
	engine.WithModelRouter(router)
	engine.WithPhase5(trainingPool, appealStore, reputationMgr, versionMgr)
	engine.WithHooks(preHooks, postHooks, stopHooks)

	stores := &engineStores{
		reviewStore: reviewStore, trainingPool: trainingPool,
		appealStore: appealStore, reputationMgr: reputationMgr,
		versionMgr: versionMgr, recheckScheduler: recheckScheduler,
		budget: budget, auditHook: auditHook,
		router: router,
	}
	return engine, stores
}

func printBanner() {
	fmt.Println("\n╔══════════════════════════════════════════════════════╗")
	fmt.Println("║  AdGuard Agent — Ad Content Safety Review System     ║")
	fmt.Println("║  Multi-Agent  |  6 tools  |  Agent Hooks             ║")
	fmt.Println("╚══════════════════════════════════════════════════════╝")
}

func printReviewResult(ad *types.AdContent, result *agent.LoopResult, expected string, stores *engineStores) {
	if result.ReviewResult == nil {
		fmt.Printf("  (no result — %s)\n", result.ExitReason)
		return
	}
	rr := result.ReviewResult

	// Per-specialist decisions already printed in real-time via orchestrator progress callback.
	// Show streaming metrics if available.
	if sm := result.State.StreamMetrics; sm != nil && sm.ToolsDispatched > 0 {
		fmt.Printf("  Streaming: %d tools, collect_wait=%s\n",
			sm.ToolsDispatched, sm.CollectWait.Round(time.Microsecond))
	}

	// Show verification status.
	if rec, ok := stores.reviewStore.Get(ad.ID); ok && rec.VerificationStatus != "" {
		fmt.Printf("  Verification: %s\n", rec.VerificationStatus)
	}

	// Show recheck scheduling.
	if stores.recheckScheduler != nil && stores.recheckScheduler.PendingCount() > 0 {
		if rr.Decision == types.DecisionPassed {
			fmt.Printf("  Recheck: 24h scheduled (high-risk PASSED)\n")
		}
	}

	// Final decision line.
	fmt.Printf("  → %s  conf=%.2f  %s  (expected: %s)\n",
		rr.Decision, rr.Confidence,
		rr.ReviewDuration.Round(time.Millisecond), expected)
}

func printFeatureShowcase(stores *engineStores, client llm.LLMClient) {
	fmt.Println("\n=== Feature Showcase ===")

	// 1. Graceful Shutdown
	fmt.Println("  ✓ Graceful Shutdown     SIGINT/SIGTERM → wait in-flight → flush JSONL → 5s failsafe")

	// 2. JSONL Persistence
	reviewCount := stores.reviewStore.JSONLCount()
	if reviewCount > 0 {
		fmt.Printf("  ✓ JSONL Persistence     %d reviews persisted (crash-safe, append-only)\n", reviewCount)
	} else {
		fmt.Println("  ✓ JSONL Persistence     enabled in real LLM mode (crash-safe, append-only)")
	}

	// 3. Model Routing
	fmt.Println("  ✓ Model Routing         per-pipeline×role routing + 529 cross-provider fallback")

	// 4. Tool Result Budget
	fmt.Println("  ✓ Tool Result Budget    2-layer: per-tool 32KB + per-round 200KB, disk fallback")

	// 5. Streaming Executor
	fmt.Println("  ✓ Streaming Executor    tools dispatch during LLM stream (channel+goroutine)")

	// 6. Strategy A/B
	comp, err := strategy.Compare(stores.versionMgr, stores.reviewStore, strategy.DefaultABConfig(), nil)
	if err == nil {
		fmt.Printf("  ✓ Strategy A/B          %s vs %s → %s\n",
			comp.ActiveVersion, comp.CanaryVersion, comp.Recommendation)
	} else {
		fmt.Printf("  ✓ Strategy A/B          %s\n", err)
	}

	// 7. Scheduled Recheck
	if stores.recheckScheduler != nil {
		rs := stores.recheckScheduler.Stats()
		fmt.Printf("  ✓ Scheduled Recheck     %d pending, %d completed\n", rs.Pending, rs.Completed)
	}

	// 8. Active Learning
	if stores.trainingPool != nil {
		ts := stores.trainingPool.Stats()
		fmt.Printf("  ✓ Active Learning       %d training samples (%d high-priority boundary cases)\n",
			ts.Total, ts.HighPriorityCount)
	}

	// 9. Tool Hooks (audit trail + circuit-breaker protection)
	if stores.auditHook != nil {
		entries := stores.auditHook.Entries()
		fmt.Printf("  ✓ Tool Hooks            %d audit entries (pre+post tool execution)\n", len(entries))
	}

	// Cost
	if client != nil {
		cost := client.Usage().TotalCost()
		if cost > 0 {
			fmt.Printf("\n  Total Cost: $%.4f\n", cost)
		}
	}
}

func demoAppeal(ctx context.Context, engine *agent.ReviewEngine, stores *engineStores, samples []types.TestAdSample, logger *slog.Logger) {
	rejected := stores.reviewStore.QueryByDecision(types.DecisionRejected)
	if len(rejected) == 0 {
		return
	}
	rec := rejected[0]
	appeal, err := stores.appealStore.Submit(rec.AdID, rec.AdvertiserID, "We believe this ad complies with all policies")
	if err != nil {
		return
	}

	// Find the original AdContent.
	var ad *types.AdContent
	for i := range samples {
		if samples[i].AdContent.ID == rec.AdID {
			ad = &samples[i].AdContent
			break
		}
	}
	if ad == nil {
		return
	}

	outcome, _ := engine.ProcessAppeal(ctx, ad, rec, appeal)
	fmt.Printf("\n  Appeal: %s → %s\n", rec.AdID, outcome)
}

func demoVersionManager(vm *strategy.VersionManager, logger *slog.Logger) {
	vm.Create("v2.0")
	vm.Deploy("v2.0", 10)
	fmt.Printf("\n  Version: v1.0 active, v2.0 canary (10%%)\n")
}

type verifyStatsResult struct{ total, agree, disagree int }

func simulateVerification(rs *store.ReviewStore, tp *store.TrainingPool, matrix *strategy.StrategyMatrix, logger *slog.Logger) verifyStatsResult {
	var stats verifyStatsResult
	rejected := rs.QueryByDecision(types.DecisionRejected)
	for _, rec := range rejected {
		plan := matrix.GetReviewPlan(rec.Region, rec.Category)
		if !plan.RequireVerification {
			continue
		}
		stats.total++
		if rec.Confidence >= 0.8 {
			rs.UpdateVerification(rec.AdID, store.VerificationConfirmed, rec.Decision, "simulated: high confidence")
			stats.agree++
		} else {
			rs.UpdateVerification(rec.AdID, store.VerificationOverride, types.DecisionManualReview, "simulated: low confidence")
			stats.disagree++
			tp.Add(&store.TrainingRecord{
				AdID: rec.AdID, Source: store.SourceVerificationOverride,
				OriginalDecision: rec.Decision, FinalDecision: types.DecisionManualReview,
				Region: rec.Region, Category: rec.Category,
			})
		}
	}
	return stats
}


func loadSamples(path string) ([]types.TestAdSample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading samples: %w", err)
	}
	var samples []types.TestAdSample
	if err := json.Unmarshal(data, &samples); err != nil {
		return nil, fmt.Errorf("parsing samples: %w", err)
	}
	return samples, nil
}
