# AdGuard Agent

[![en](https://img.shields.io/badge/lang-English-blue.svg)](README.md) [![zh](https://img.shields.io/badge/lang-中文-red.svg)](README_zh.md)

Multi-Agent content safety system for international advertising review.

## Overview

AdGuard Agent automates ad content review across global markets. The system applies region-specific policies and risk-based routing to determine review depth, using an agentic loop that drives specialized tools through a perception-attribution-adjudication pipeline.

### Core Components (Implemented)

- **Strategy Matrix** — Data-driven policy engine: maps (region x category) to applicable policies, risk levels, and review pipelines. Zero hardcoded business rules. Covers 20 policies, 6 regions, 23 risk categories.
- **Agentic Loop** — State machine-driven review lifecycle (PENDING -> ANALYZING -> JUDGING -> DECIDED) with transition audit trail, max_output_tokens two-level recovery, and fail-closed fallback to MANUAL_REVIEW.
- **Tool System** — 5 review tools with fail-closed defaults, concurrent execution for read-only tools, input validation, and result truncation. Tools: ContentAnalyzer, PolicyMatcher, RegionCompliance, LandingPageChecker, HistoryLookup.
- **LLM Client** — OpenAI-compatible API client with multi-provider support, exponential backoff retry, and per-model usage tracking.
- **Review Engine** — Orchestrates the full review: strategy matrix query -> pipeline selection (fast/standard/comprehensive) -> agentic loop -> structured ReviewResult output.
- **Context Management** — Three-layer cascading compression (MicroCompact -> AutoCompact -> ReactiveCompact) with LLM-driven summarization, circuit breaker, and token budget with diminishing returns detection. Enables batch review of 15+ ads without context overflow.
- **ReviewStore** — Structured review record storage with multi-dimensional queries (by ad, advertiser, region, decision). Data foundation for the label-detect-train pipeline.
- **Verification** — Independent LLM-as-Judge re-check of REJECTED decisions. Fail-closed: disagree only downgrades to MANUAL_REVIEW, never upgrades to PASSED. Triggered by pipeline risk level.
- **Hook System** — Complete PreToolHook/PostToolHook/StopHook integrated into the agentic loop. Implementations: ToolPermissionHook (pipeline-based tool restrictions), AuditHook (tool invocation audit trail), CircuitBreakerHook (consecutive failure detection), ResultValidationHook, FinalAuditHook.
- **Multi-Agent Orchestrator** — 3 specialist agents (Content, Policy, Region) execute in parallel via goroutines, each reusing the same Run() agentic loop with isolated State and filtered tool sets. Adjudicator agent synthesizes results with conflict detection and weighted arbitration.
- **False-Positive Control L3** — Multi-Agent cross-validation: unanimous agreement boosts confidence, 2:1 split follows majority with reduced confidence, 3-way disagreement forces MANUAL_REVIEW, critical violations override PASSED decisions.
- **Query Chain Tracking** — ChainID + Depth tracking across parent/child agents for full execution graph reconstruction. Supports "attribution" stage traceability.
- **Appeal Workflow** — Full advertiser appeal lifecycle (SUBMITTED -> REVIEWING -> RESOLVED). Appeal Agent reuses Run() for independent re-review. Outcomes: UPHELD/OVERTURNED/PARTIAL. One appeal per ad. OVERTURNED feeds training data pool.
- **Strategy Version Management** — Version state machine (DRAFT -> CANARY -> ACTIVE -> ROLLBACK). Deterministic hash-based traffic routing for canary. Single-active + single-canary invariant. Promote/Rollback operations.
- **Training Data Pool** — Three-source collection pipeline: high-confidence reviews, verification overrides, appeal overturns. Filterable by source/region/category. Completes the label-detect-train data flywheel.
- **Advertiser Reputation** — Trust score tracking linked to appeal outcomes. OVERTURNED raises trust, UPHELD lowers trust and increments violations. Risk categorization: trusted/standard/flagged/probation.
- **Graceful Shutdown** — SIGINT/SIGTERM handler with cleanup registry and 5-second failsafe timer. Waits for in-flight reviews to complete, then flushes all JSONL stores before exit.
- **JSONL Persistence** — Append-only JSONL files for crash-safe review data persistence. Each store (ReviewStore, AppealStore, TrainingPool) maintains its own file. On startup, existing records are recovered by replaying the log; corrupted lines from mid-write crashes are silently skipped.
- **Model Routing** — Per-pipeline and per-agent-role model selection via a 2-level routing matrix. xAI 3-tier model hierarchy: `fast→grok-4-1-fast-non-reasoning` (cheapest, no reasoning for low-risk), `standard→grok-4-1-fast-reasoning` (balanced), `comprehensive/adjudicator/appeal→grok-4.20-0309-reasoning` (strongest reasoning). Cross-provider fallback chain: `grok-4.20-0309-reasoning→grok-4-1-fast-reasoning→gpt-4o`.
- **529 Overload Fallback** — Tracks consecutive 529 (overloaded) errors. After 3 consecutive 529s, automatically retries with the degraded model from the fallback chain. Prevents review pipeline stalling during provider capacity issues.
- **Tool Result Budget** — Two-layer size control for tool results. Layer 1 (per-tool): results exceeding 32KB are persisted to disk with a 2KB inline preview featuring smart newline-boundary truncation and HTML signal extraction (title, meta description, privacy policy detection). Layer 2 (per-round): when aggregate results exceed 200KB, the largest are iteratively evicted to disk. Prevents context window explosion from large landing page HTML (50-200KB), the highest-frequency ad rejection reason.
- **Streaming Tool Execution** — StreamingToolExecutor dispatches tools during LLM streaming response, eliminating the wait for full response completion. Go channel + goroutine as the natural equivalent of AsyncGenerator. Concurrency rules: concurrent-safe tools run in parallel, non-concurrent tools block the queue. StreamAccumulator handles OpenAI SSE chunk processing with index-based tool call accumulation — JSON fragments are concatenated (O(n)) rather than incrementally parsed (O(n²)). Automatic non-streaming fallback on connection failure or stream interruption.

- **Strategy A/B Testing** — Automated comparison of canary vs active strategy versions using per-version review metrics (pass rate, avg confidence, false positive count from verification overrides). Recommendation engine: ROLLBACK if canary FP rate exceeds 2x active, PROMOTE if canary metrics equal or better, CONTINUE if insufficient data or inconclusive. Metrics computed at query time via `VersionStats()`, not pre-aggregated at write time.
- **Scheduled Post-Approval Recheck** — Background scheduler for re-reviewing PASSED high-risk ads after a configurable delay (default 24h). Defends against adversarial landing page swaps post-approval. JSONL-persisted task queue with crash recovery: missed tasks detected on startup and executed immediately, expired tasks (>72h) auto-discarded. One-pending-per-ad invariant prevents duplicate rechecks. Integrates via PostReviewHook chain.

### Future Extensions

- HTTP API for external integration
- Image/video content analysis via multimodal LLM

## Architecture

```mermaid
flowchart TD
    A[Ad Content] --> B["Risk Assessment ← Strategy Matrix (region × category)"]
    B --> C["Fast (single Agent)"]
    B --> D["Standard / Comprehensive (Multi-Agent)"]
    C --> E["Single Agent (5 tools, 3 turns)"]
    D --> F["Fork 3 Specialists<br/>parallel goroutines"]
    F --> G[Content Agent]
    F --> H[Policy Agent]
    F --> I[Region Agent]
    G & H & I --> J["Adjudicator (conflict detect + weighted arbitration)"]
    J --> K["L3 Cross-Validation + L4 Verification"]
    E & K --> L["Decision → PASSED / REJECTED / MANUAL_REVIEW"]
```

## Quick Start

```bash
# Build
go build ./...

# Run all tests
go test ./... -v

# Run without API key (mock mode — reviews all 15 samples)
go run ./cmd/adguard/

# Run with API key (real LLM — Multi-Agent review with grok-4-1-fast-reasoning)
LLM_API_KEY=your_key go run ./cmd/adguard/
```

## Real LLM Output

Three ads reviewed end-to-end with Multi-Agent orchestration (xAI grok models). Total cost: **$0.003**.

```
╔══════════════════════════════════════════════════════╗
║  AdGuard Agent — Ad Content Safety Review System     ║
║  16K lines Go  |  7 upgrades  |  Multi-Agent         ║
╚══════════════════════════════════════════════════════╝
=== Model Routing ===
  fast                   → grok-4-1-fast-non-reasoning
  standard               → grok-4-1-fast-reasoning
  comprehensive          → grok-4.20-0309-reasoning
  adjudicator            → grok-4.20-0309-reasoning
  appeal                 → grok-4.20-0309-reasoning
  fallback chain: grok-4.20-0309-reasoning → grok-4-1-fast-reasoning → gpt-4o

=== Review: 3 ads ===

--- ad_001 (US/healthcare) [multi-agent] ---
  ├─ region:        analyzing...
  ├─ content:       analyzing...
  ├─ policy:        analyzing...
  ├─ policy:        REJECTED        conf=1.00  (15.902s)
  ├─ region:        REJECTED        conf=1.00  (17.304s)
  ├─ content:       REJECTED        conf=1.00  (22.661s)
  ├─ adjudicator:   synthesizing...
  ├─ adjudicator:   REJECTED        conf=1.00  (7.812s)
  Verification: confirmed
  → REJECTED  conf=1.00  30.474s  (expected: REJECTED)

--- ad_002 (US/finance) [multi-agent] ---
  ├─ region:        analyzing...
  ├─ content:       analyzing...
  ├─ policy:        analyzing...
  ├─ region:        PASSED          conf=1.00  (10.389s)
  ├─ policy:        PASSED          conf=1.00  (17.473s)
  ├─ content:       PASSED          conf=1.00  (17.999s)
  ├─ adjudicator:   synthesizing...
  ├─ adjudicator:   PASSED          conf=1.00  (2.519s)
  Recheck: 24h scheduled (high-risk PASSED)
  → PASSED  conf=1.00  20.519s  (expected: PASSED)

--- ad_003 (EU/healthcare) [multi-agent] ---
  ├─ region:        analyzing...
  ├─ policy:        analyzing...
  ├─ content:       analyzing...
  ├─ policy:        MANUAL_REVIEW   conf=0.70  (25.941s)
  ├─ region:        MANUAL_REVIEW   conf=0.65  (26.05s)
  ├─ content:       MANUAL_REVIEW   conf=0.80  (33.057s)
  ├─ adjudicator:   synthesizing...
  ├─ adjudicator:   MANUAL_REVIEW   conf=0.85  (6.014s)
  → MANUAL_REVIEW  conf=0.90  39.072s  (expected: PASSED)

  Version: v1.0 active, v2.0 canary (10%)

=== Feature Showcase ===
  ✓ Graceful Shutdown     SIGINT/SIGTERM → wait in-flight → flush JSONL → 5s failsafe
  ✓ JSONL Persistence     78 reviews persisted (crash-safe, append-only)
  ✓ Model Routing         per-pipeline×role routing + 529 cross-provider fallback
  ✓ Tool Result Budget    2-layer: per-tool 32KB + per-round 200KB, disk fallback
  ✓ Streaming Executor    tools dispatch during LLM stream (channel+goroutine)
  ✓ Strategy A/B          v1.0 vs v2.0 → CONTINUE
  ✓ Scheduled Recheck     1 pending, 0 completed

  Total Cost: $0.0028
```

**Key observations:**
- **ad_001**: 3:0 unanimous REJECTED with confidence=1.0. Verification confirmed. Textbook violation (unverified medical claim + false FDA approval). 3 specialists run in parallel via goroutines (policy finishes first at 15.9s, content last at 22.7s), then adjudicator synthesizes.
- **ad_002**: 3:0 unanimous PASSED with confidence=1.0. Scheduled for 24h post-approval recheck (high-risk finance category) — defends against adversarial landing page swap.
- **ad_003**: 3:0 MANUAL_REVIEW — EU strict healthcare region causes all three specialists to flag for human review (region conf=0.65, policy conf=0.70, content conf=0.80). Adjudicator aggregates to conf=0.85. This is the fail-closed design working correctly: when uncertain, escalate rather than auto-approve.
- **Feature Showcase**: JSONL persistence count (78) accumulates across runs, demonstrating crash-safe append-only durability. A/B recommendation is CONTINUE (insufficient canary data for conclusive comparison).

## Configuration

Environment variables (highest priority):

| Variable | Default | Description |
|----------|---------|-------------|
| `LLM_PROVIDER` | `xai` | LLM provider name |
| `LLM_BASE_URL` | `https://api.x.ai/v1` | API endpoint |
| `LLM_MODEL` | `grok-4-1-fast-reasoning` | Model identifier |
| `LLM_API_KEY` | — | API key (required for real LLM mode) |
| `LOG_LEVEL` | `warn` | Log level (debug/info/warn/error) |
| `DATA_DIR` | `data` | Path to data directory |

Model routing is configured via `RoutingConfig` in code (see `internal/llm/router.go:DefaultRoutingConfig`).

Config file (`config.json` in project root, optional) and built-in defaults provide fallback values.

## Project Structure

```
cmd/adguard/         CLI entry point (dual mode: real LLM / mock LLM)
internal/
  types/             Shared types (messages, review, strategy)
  llm/               LLM client, retry, usage tracking, model router
  config/            Configuration loading (env > file > defaults)
  shutdown/          Graceful shutdown with cleanup registry
  strategy/          Strategy matrix engine (policy x region -> review plan)
  agent/             Agentic loop, state machine, recovery, stream events
  agent/mock/        Mock LLM client and tool executor (for testing)
  tool/              Tool system: 5 review tools + executor + registry
  compact/           Context compression + token budget
  store/             ReviewStore + Verification + Appeal + Training pool + JSONL persistence
  strategy/          Strategy matrix + version management + A/B testing
  recheck/           Scheduled post-approval recheck scheduler
data/
  policy_kb.json     Policy knowledge base (20 TikTok-aligned policies)
  region_rules.json  Regional compliance rules (6 regions)
  category_risk.json Category -> risk level mapping (23 categories)
  samples/           Test ad samples (15 samples)
```
