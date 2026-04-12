# AdGuard Agent

[![en](https://img.shields.io/badge/lang-English-blue.svg)](README.md) [![zh](https://img.shields.io/badge/lang-中文-red.svg)](README_zh.md)

Multi-Agent content safety system for international ad review, built in Go.

## Key Capabilities

- **Coordinator-driven orchestration**: A Coordinator agent dynamically dispatches specialist agents (content, policy, region) and synthesizes their findings into a final decision — not a static fork-join pipeline.
- **6 review tools**: Content analysis, policy matching, region compliance, landing page verification, advertiser history, and on-demand policy KB lookup.
- **4-layer false-positive control**: Historical consistency (L1), confidence thresholds (L2), multi-agent cross-validation (L3), and adversarial verification (L4).
- **LLM-driven memory extraction**: After each review, an extraction agent analyzes the review context and autonomously decides what patterns are worth remembering (advertiser behavior, Algospeak variants, policy precedents, regional edge cases).
- **Anti-confirmation-bias verification**: Adversarial L4 verifier forces counterarguments and pre-debunks common rationalization patterns before judging REJECTED decisions.
- **Appeal agent with investigation tools**: Appeal agents can independently re-verify advertiser claims using landing page checks, policy lookups, and history queries.
- **System Monitor**: Post-batch anomaly detection — rejection rate spikes, advertiser violation clustering, policy hotspots, verification override trends.
- **Streaming tool execution**: Tools dispatch during LLM streaming via JSON boundary detection, with concurrent-safe parallel execution.
- **Data-driven strategy matrix**: Zero hardcoded business rules — all policies, region rules, and category risk levels loaded from JSON configuration.
- **Context compression**: Three-layer cascading compression (MicroCompact, AutoCompact, ReactiveCompact) for long review sessions.
- **Graceful shutdown**: SIGINT/SIGTERM handler + cleanup registry + 5s failsafe timer. Waits for in-flight reviews before flushing JSONL.

## Architecture

```
Coordinator (agentic loop)
  |-- dispatch_specialist("content") --> ContentAgent (analyze_content, check_landing_page)
  |-- dispatch_specialist("policy")  --> PolicyAgent  (match_policies, query_policy_kb)
  |-- dispatch_specialist("region")  --> RegionAgent  (check_region_compliance, query_policy_kb, lookup_history)
  |
  v
Coordinator synthesizes --> Final ReviewResult --> L3 safety net --> Verification (L4)
                                                                --> PostReview hooks (store, training, recheck, memory)
```

## Quick Start

```bash
# Mock mode (no API key needed)
go run ./cmd/adguard/

# Real LLM mode (xAI Grok)
LLM_API_KEY=your-key go run ./cmd/adguard/

# Tests
go test ./...
```

## Project Structure

```
cmd/adguard/           CLI entry point, demo wiring
internal/
  agent/               Agentic loop, coordinator orchestration, hooks, state machine, memory extraction, monitor
  tool/                6 review tools + tool registry + result budget
  store/               ReviewStore, AppealStore, TrainingPool, Verifier (JSONL persistence)
  memory/              Per-role agent memory with JSONL persistence
  llm/                 LLM client (OpenAI-compatible), model routing, retry
  strategy/            Strategy matrix (policy x region -> review plan), A/B versioning
  compact/             Context compression (MicroCompact, AutoCompact, ReactiveCompact)
  recheck/             Scheduled post-approval re-review
  config/              Three-tier configuration (env > file > defaults)
  types/               Shared type definitions
data/
  policy_kb.json       Policy knowledge base (21 policies)
  region_rules.json    Region x category compliance matrix
  all_samples.json     15 test ad samples with expected outcomes
  real-llm-runs/       Archived real LLM test outputs
```

## Demo Output (Real LLM)

```
--- ad_001 (US/healthcare) [multi-agent] ---
  |-- coordinator:   directing review...
  |-- content:       analyzing...
  |-- policy:        analyzing...
  |-- region:        analyzing...
  |-- content:       REJECTED        conf=1.00  (17.5s)
  |-- region:        REJECTED        conf=0.98  (20.8s)
  |-- policy:        REJECTED        conf=0.95  (21.1s)
  |-- coordinator:   REJECTED        conf=1.00  (33.8s)
  Verification: override
  -> MANUAL_REVIEW  conf=1.00  33.8s  (expected: REJECTED)

=== Monitor Report ===
  Reviews: 3 | Rejection rate: 33% | Avg confidence: 0.92 | Override rate: 100%
  Recommendations:
    - Override rate 100% appears high but sample size is too small (1 verification)

=== Feature Showcase ===
  Graceful Shutdown, JSONL Persistence, Model Routing, Tool Result Budget,
  Streaming Executor, Strategy A/B, Scheduled Recheck, Active Learning,
  Tool Hooks (162 audit entries), Agent Memory (15 entries), System Monitor
```

## Tech Stack

- Go 1.22+, zero external dependencies (stdlib only)
- OpenAI-compatible LLM API (tested with xAI Grok)
- JSONL append-only persistence (crash-safe, WAL pattern — duplicate entries per ad ID are normal, latest wins on recovery)
