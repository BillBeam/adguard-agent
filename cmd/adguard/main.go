package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"sync"

	"github.com/BillBeam/adguard-agent/internal/agent"
	"github.com/BillBeam/adguard-agent/internal/agent/mock"
	"github.com/BillBeam/adguard-agent/internal/compact"
	"github.com/BillBeam/adguard-agent/internal/config"
	"github.com/BillBeam/adguard-agent/internal/llm"
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

	// Display routing table.
	fmt.Print(stores.router.FormatRoutingTable())
	fmt.Printf("\n=== Real LLM Review (%d ads) ===\n\n", limit)
	for i := 0; i < limit; i++ {
		ad := &samples[i].AdContent
		stores.budget.ResetForReview()
		reviewWg.Add(1)
		result, reviewErr := engine.Review(context.Background(), ad)
		reviewWg.Done()
		if reviewErr != nil {
			logger.Error("review failed", "ad_id", ad.ID, "error", reviewErr)
			continue
		}
		printReviewResult(ad, result, samples[i].ExpectedResult, stores)
	}

	// Appeal demo: first REJECTED ad.
	demoAppeal(context.Background(), engine, stores, samples, logger)

	// Version management demo.
	demoVersionManager(stores.versionMgr, logger)

	printReport(stores, client)
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
	auditHook := agent.NewAuditHook(logger)

	// Setup version: v1.0 active.
	versionMgr.Create("v1.0")
	versionMgr.Deploy("v1.0", 0)
	versionMgr.Promote("v1.0")

	// Model routing (same config as real LLM mode).
	router := llm.NewModelRouter(llm.DefaultRoutingConfig(), logger)
	fmt.Print(router.FormatRoutingTable())

	for i := range samples {
		ad := &samples[i].AdContent
		client := mock.NewLLMClient()
		reg := tool.NewReviewRegistry(client, matrix, logger)
		executor := tool.NewExecutor(reg, logger)

		hookChain := agent.NewHookChain(logger).Add(executor).Add(reviewStore).Add(trainingPool)
		orchestrator := agent.NewOrchestrator(client, matrix, reg, logger)
		orchestrator.WithModelRouter(router)

		engine := agent.NewReviewEngine(client, matrix, reg.ExportDefinitions(), executor, logger, hookChain)
		engine.WithOrchestrator(orchestrator)
		engine.WithModelRouter(router)
		engine.WithPhase3(nil, nil, reviewStore, nil)
		engine.WithPhase5(trainingPool, appealStore, reputationMgr, versionMgr)

		reviewWg.Add(1)
		result, reviewErr := engine.Review(context.Background(), ad)
		reviewWg.Done()
		if reviewErr != nil {
			logger.Error("review failed", "ad_id", ad.ID, "error", reviewErr)
			continue
		}
		if result.ReviewResult == nil {
			continue
		}

		plan := matrix.GetReviewPlan(ad.Region, ad.Category)
		mode := "single"
		if plan.Pipeline != "fast" && len(plan.RequiredAgents) > 1 {
			mode = "multi"
		}
		fmt.Printf("  %-10s  %-12s  %-15s  expected=%-15s  [%s/%s]\n",
			ad.ID, ad.Region, result.ReviewResult.Decision, samples[i].ExpectedResult,
			plan.Pipeline, mode)
	}

	// Simulate verification.
	verifyStats := simulateVerification(reviewStore, trainingPool, matrix, logger)

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

	// Report.
	fmt.Println()
	printStoreStats(reviewStore)

	fmt.Printf("\nVerification: %d checked (%d agree, %d disagree)\n",
		verifyStats.total, verifyStats.agree, verifyStats.disagree)

	appealStats := appealStore.Stats()
	fmt.Printf("Appeals: %d total (%d upheld, %d overturned)\n",
		appealStats.Total, appealStats.ByOutcome[store.AppealUpheld], appealStats.ByOutcome[store.AppealOverturned])

	tpStats := trainingPool.Stats()
	fmt.Printf("Training Pool: %d records (review=%d, verification=%d, appeal=%d)\n",
		tpStats.Total, tpStats.BySource[store.SourceReview],
		tpStats.BySource[store.SourceVerificationOverride],
		tpStats.BySource[store.SourceAppealOverturn])

	if active, ok := versionMgr.GetActive(); ok {
		fmt.Printf("Strategy Version: active=%s", active.VersionID)
		if canary, ok := versionMgr.GetCanary(); ok {
			fmt.Printf(" canary=%s (%d%%)", canary.VersionID, canary.TrafficPct)
		}
		fmt.Println()
	}

	fmt.Printf("Audit: %d hook entries\n", len(auditHook.Entries()))
}

// --- Shared helpers ---

type engineStores struct {
	reviewStore   *store.ReviewStore
	trainingPool  *store.TrainingPool
	appealStore   *store.AppealStore
	reputationMgr *store.ReputationManager
	versionMgr    *strategy.VersionManager
	budget        *compact.TokenBudget
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

	reg := tool.NewReviewRegistry(client, matrix, logger)
	executor := tool.NewExecutor(reg, logger)

	hookChain := agent.NewHookChain(logger).Add(executor).Add(reviewStore).Add(trainingPool)

	// Model routing: per-pipeline and per-role model selection.
	router := llm.NewModelRouter(llm.DefaultRoutingConfig(), logger)
	orchestrator := agent.NewOrchestrator(client, matrix, reg, logger)
	orchestrator.WithModelRouter(router)

	ctxMgr := compact.NewContextManager(compact.DefaultCompactConfig(), client, logger)
	budget := compact.NewTokenBudget(compact.DefaultBudgetConfig())

	// Setup version: v1.0 active.
	versionMgr.Create("v1.0")
	versionMgr.Deploy("v1.0", 0)
	versionMgr.Promote("v1.0")

	engine := agent.NewReviewEngine(client, matrix, reg.ExportDefinitions(), executor, logger, hookChain)
	engine.WithPhase3(ctxMgr, budget, reviewStore, verifier)
	engine.WithOrchestrator(orchestrator)
	engine.WithModelRouter(router)
	engine.WithPhase5(trainingPool, appealStore, reputationMgr, versionMgr)

	stores := &engineStores{
		reviewStore: reviewStore, trainingPool: trainingPool,
		appealStore: appealStore, reputationMgr: reputationMgr,
		versionMgr: versionMgr, budget: budget, auditHook: auditHook,
		router: router,
	}
	return engine, stores
}

func printReviewResult(ad *types.AdContent, result *agent.LoopResult, expected string, stores *engineStores) {
	plan := fmt.Sprintf("[%s]", ad.Region)
	if result.ReviewResult != nil {
		fmt.Printf("  %-10s  %-12s  %-15s  conf=%.2f  expected=%s",
			ad.ID, ad.Region, result.ReviewResult.Decision, result.ReviewResult.Confidence, expected)
		if rec, ok := stores.reviewStore.Get(ad.ID); ok && rec.VerificationStatus != "" {
			fmt.Printf("  verify=%s", rec.VerificationStatus)
		}
		fmt.Println()
	} else {
		fmt.Printf("  %-10s  %s  (none — %s)\n", ad.ID, plan, result.ExitReason)
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

func printReport(stores *engineStores, client llm.LLMClient) {
	fmt.Println()
	printStoreStats(stores.reviewStore)

	tpStats := stores.trainingPool.Stats()
	fmt.Printf("\nTraining Pool: %d records (review=%d, verification=%d, appeal=%d)\n",
		tpStats.Total, tpStats.BySource[store.SourceReview],
		tpStats.BySource[store.SourceVerificationOverride],
		tpStats.BySource[store.SourceAppealOverturn])

	appealStats := stores.appealStore.Stats()
	if appealStats.Total > 0 {
		fmt.Printf("Appeals: %d total (%d upheld, %d overturned)\n",
			appealStats.Total, appealStats.ByOutcome[store.AppealUpheld], appealStats.ByOutcome[store.AppealOverturned])
	}

	if client != nil {
		fmt.Printf("\nCost: $%.6f\n", client.Usage().TotalCost())
	}
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

func printStoreStats(rs *store.ReviewStore) {
	stats := rs.Stats()
	fmt.Printf("=== ReviewStore Summary (%d reviews) ===\n", stats.Total)
	fmt.Printf("  PASSED:        %d\n", stats.ByDecision[types.DecisionPassed])
	fmt.Printf("  REJECTED:      %d\n", stats.ByDecision[types.DecisionRejected])
	fmt.Printf("  MANUAL_REVIEW: %d\n", stats.ByDecision[types.DecisionManualReview])
	if stats.Total > 0 {
		fmt.Printf("  Avg confidence: %.2f | Pass rate: %.1f%%\n", stats.AverageConfidence, stats.PassRate*100)
	}
	if len(stats.ByPipeline) > 0 {
		fmt.Printf("  Pipelines:     ")
		first := true
		for p, n := range stats.ByPipeline {
			if !first {
				fmt.Printf(", ")
			}
			fmt.Printf("%s=%d", p, n)
			first = false
		}
		fmt.Println()
	}
	if stats.VerifiedCount > 0 {
		fmt.Printf("  Verified:      %d (%d agree, %d override)\n",
			stats.VerifiedCount, stats.VerifiedCount-stats.OverrideCount, stats.OverrideCount)
	}
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
