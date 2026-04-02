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

### Future Extensions

- HTTP API for external integration
- Persistent storage backend (PostgreSQL/Redis)
- Real-time monitoring dashboard
- Image/video content analysis via multimodal LLM

## Architecture

```
                    ┌─────────────┐
                    │  Ad Content  │
                    └──────┬──────┘
                           │
                    ┌──────▼──────┐
                    │ Risk Assess  │  <- Strategy Matrix (region x category)
                    └──────┬──────┘
                           │
              ┌────────────┴────────────────────────┐
              ▼                                     ▼
        ┌──────────┐                    ┌───────────────────┐
        │   Fast   │                    │ Standard / Compr. │
        │ (single) │                    │    (multi-agent)  │
        └────┬─────┘                    └─────────┬─────────┘
             │                                    │
     ┌───────▼────────┐               ┌───────────▼───────────┐
     │  Single Agent   │               │  Fork 3 Specialists   │
     │  (5 tools,      │               │  (parallel goroutines) │
     │   3 turns)      │               ├────────┬──────┬───────┤
     └───────┬────────┘               │Content │Policy│Region │
             │                         │Agent   │Agent │Agent  │
             │                         └────┬───┴──┬───┴───┬───┘
             │                              └──────┼───────┘
             │                           ┌─────────▼─────────┐
             │                           │   Adjudicator     │
             │                           │ (conflict detect + │
             │                           │  weighted arbitr.) │
             │                           └─────────┬─────────┘
             │                                     │
             │                           ┌─────────▼─────────┐
             │                           │  L3 Cross-Valid.   │
             │                           │  + L4 Verification │
             │                           └─────────┬─────────┘
             │                                     │
             └──────────────┬──────────────────────┘
                            ▼
                   ┌────────────────┐
                   │    Decision    │ -> PASSED / REJECTED / MANUAL_REVIEW
                   └────────────────┘
```

## Quick Start

```bash
# Build
go build ./...

# Run all tests (151 tests)
go test ./... -v

# Run without API key (mock mode — reviews all 15 samples)
go run ./cmd/adguard/

# Run with API key (real LLM — Multi-Agent review with grok-4-1-fast-reasoning)
LLM_API_KEY=your_key go run ./cmd/adguard/
```

## Real LLM Output (grok-4-1-fast-reasoning)

Three ads reviewed end-to-end with Multi-Agent orchestration. Total cost: **$0.15**.

```
=== Real LLM Review (3 ads) ===

--- ad_001 (US/healthcare) — "Miracle Cure for Diabetes, FDA Approved" ---
Pipeline: standard/multi-agent
  ContentAgent:  REJECTED  conf=1.00  (33.9s — detected unverified medical claims)
  PolicyAgent:   REJECTED  conf=1.00  (15.0s — matched POL_001, POL_002)
  RegionAgent:   REJECTED  conf=1.00  (14.3s — landing page compliance issues)
  Adjudicator:   REJECTED  conf=1.00  (3:0 unanimous)
  Verification:  confirmed ✓
  → Final: REJECTED  conf=1.00  (expected: REJECTED ✓)

--- ad_002 (US/finance) — "Premium Investment Advisory Services" ---
Pipeline: standard/multi-agent
  ContentAgent:  PASSED    conf=1.00  (compliant financial services copy)
  PolicyAgent:   PASSED    conf=0.95  (meets disclosure requirements)
  RegionAgent:   MANUAL    conf=0.85  (flagged for additional regional check)
  Adjudicator:   PASSED    conf=0.85  (2:1 majority PASSED, L3 reduced confidence)
  → Final: PASSED   conf=0.72  (expected: PASSED ✓)

--- ad_003 (EU/healthcare) — "Clinical Trial Results for Joint Pain Relief" ---
Pipeline: comprehensive/multi-agent
  ContentAgent:  PASSED    conf=0.85  (claims appear substantiated)
  PolicyAgent:   PASSED    conf=0.95  (compliant with EU health claims regulation)
  RegionAgent:   MANUAL    conf=0.85  (EU strict region flagged for review)
  Adjudicator:   MANUAL    conf=0.85  (2:1, L3 reduced confidence)
  → Final: PASSED   conf=0.72  (expected: PASSED ✓)

--- Appeal: ad_001 ---
  Advertiser submitted: "We believe this ad complies with all policies"
  Appeal Agent decision: PARTIAL (recommend further review)
  → Outcome: PARTIAL

--- Strategy Version ---
  v1.0: ACTIVE (100% traffic)
  v2.0: CANARY (10% traffic)

=== ReviewStore Summary (3 reviews) ===
  PASSED: 2 | REJECTED: 1 | MANUAL_REVIEW: 0
  Avg confidence: 0.81 | Pass rate: 66.7%
  Verified: 1 (1 agree, 0 override)
  Training Pool: 1 record (high-confidence review sample)
  Appeals: 1 (PARTIAL)
  Cost: $0.15
```

**Key observations:**
- **ad_001**: 3:0 unanimous REJECTED with confidence=1.0. Verification confirmed. This is a textbook violation case (unverified medical claim + false FDA approval).
- **ad_002**: 2:1 split (Content+Policy PASSED, Region MANUAL_REVIEW). L3 cross-validation reduced confidence from 0.85 to 0.72. Conservative but correct.
- **ad_003**: Similar 2:1 split. EU strict region causes RegionAgent to flag for review. Adjudicator + L3 control produced a cautious PASSED.
- **Appeal**: ad_001 advertiser appealed. Appeal Agent independently reviewed and recommended PARTIAL (some violations valid, some debatable). The appeal system works end-to-end.

## Configuration

Environment variables (highest priority):

| Variable | Default | Description |
|----------|---------|-------------|
| `LLM_PROVIDER` | `xai` | LLM provider name |
| `LLM_BASE_URL` | `https://api.x.ai/v1` | API endpoint |
| `LLM_MODEL` | `grok-4-1-fast-reasoning` | Model identifier |
| `LLM_API_KEY` | — | API key (required for real LLM mode) |
| `LOG_LEVEL` | `info` | Log level (debug/info/warn/error) |
| `DATA_DIR` | `data` | Path to data directory |

Config file (`config.json` in project root) and built-in defaults provide fallback values.

## Project Structure

```
cmd/adguard/         CLI entry point (dual mode: real LLM / mock LLM)
internal/
  types/             Shared types (messages, review, strategy)
  llm/               LLM client, retry, usage tracking
  config/            Configuration loading (env > file > defaults)
  strategy/          Strategy matrix engine (policy x region -> review plan)
  agent/             Agentic loop, state machine, recovery, stream events
  agent/mock/        Mock LLM client and tool executor (for testing)
  tool/              Tool system: 5 review tools + executor + registry
  compact/           Context compression + token budget
  store/             ReviewStore + Verification + Appeal + Training pool
  strategy/          Strategy matrix + version management
data/
  policy_kb.json     Policy knowledge base (20 TikTok-aligned policies)
  region_rules.json  Regional compliance rules (6 regions)
  category_risk.json Category -> risk level mapping (23 categories)
  samples/           Test ad samples (15 samples)
```
