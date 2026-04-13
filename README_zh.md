# AdGuard Agent

[![en](https://img.shields.io/badge/lang-English-blue.svg)](README.md) [![zh](https://img.shields.io/badge/lang-中文-red.svg)](README_zh.md)

面向国际化广告内容安全审核的 Multi-Agent 系统，Go 实现。

## 核心能力

- **Coordinator 动态编排** — Coordinator Agent 动态派遣专家 Agent（内容/策略/地区），综合各方结果后亲自裁决。不是静态并行——Coordinator 根据 specialist 返回结果决定下一步。
- **6 个审核工具** — 内容分析、策略匹配、地区合规、落地页检查、广告主历史、策略知识库按需查询。
- **4 层误伤控制** — 历史一致性（L1）→ 置信度阈值（L2）→ Multi-Agent 交叉验证（L3）→ 对抗性 Verification（L4）。
- **LLM 驱动的记忆抽取** — 审核完成后，抽取 Agent 分析审核上下文，自主决定哪些模式值得记忆（广告主行为、Algospeak 变体、策略先例、地区边界案例）。
- **反确认偏误验证** — L4 Verifier 采用对抗性 prompt：强制生成反论 + 预驳斥常见合理化借口，防止 LLM 橡皮图章式确认。
- **Appeal Agent 独立调查** — 申诉审核可使用落地页检查、策略查询、历史查询工具独立验证广告主声明，而非纯推理。
- **系统监控（5 维感知检查）** — 批量审核后全维度健康检查：拒绝率突增、Override 率、广告主聚集、策略热点、置信度下降。无论是否触发异常均展示完整检查清单。
- **流式工具执行** — LLM 流式响应中通过 JSON 边界检测提前调度工具，并发安全的并行执行。
- **数据驱动策略矩阵** — 零硬编码业务规则，所有策略、地区规则、品类风险从 JSON 配置加载。

## 架构

```
Coordinator（agentic loop）
  |-- dispatch_specialist("content") --> 内容 Agent（analyze_content, check_landing_page）
  |-- dispatch_specialist("policy")  --> 策略 Agent（match_policies, query_policy_kb）
  |-- dispatch_specialist("region")  --> 地区 Agent（check_region_compliance, query_policy_kb, lookup_history）
  |
  v
Coordinator 综合裁决 --> ReviewResult --> L3 安全网 --> Verification（L4）
                                                    --> PostReview hooks（存储、训练、回检、记忆）
```

## 快速开始

```bash
# Mock 模式（无需 API key）
go run ./cmd/adguard/

# 真实 LLM 模式（xAI Grok）
LLM_API_KEY=your-key go run ./cmd/adguard/

# 测试
go test ./...
```

## 项目结构

```
cmd/adguard/           CLI 入口，demo 编排
internal/
  agent/               Agentic Loop、Coordinator 编排、Hook 系统、状态机、记忆抽取、系统监控
  tool/                6 个审核工具 + 工具注册表 + 结果预算
  store/               ReviewStore、AppealStore、TrainingPool、Verifier（JSONL 持久化）
  memory/              按角色的 Agent 记忆（JSONL 持久化）
  llm/                 LLM 客户端（OpenAI 兼容）、模型路由、重试
  strategy/            策略矩阵（策略 × 地区 → 审核计划）、A/B 版本管理
  compact/             上下文压缩（MicroCompact → AutoCompact → ReactiveCompact）
  recheck/             投放后定时回检调度
  config/              三层配置（env > file > defaults）
  types/               共享类型定义
data/
  policy_kb.json       策略知识库（21 条策略）
  region_rules.json    地区 × 品类合规矩阵
  all_samples.json     15 条测试广告样本
  real-llm-runs/       真实 LLM 测试输出存档
```

## Demo 输出（真实 LLM）

```
--- ad_001 (US/healthcare) [multi-agent] ---
  ├─ coordinator:   directing review...
  ├─ content:       REJECTED        conf=1.00  (18.6s)
  ├─ policy:        REJECTED        conf=1.00  (19.2s)
  ├─ region:        REJECTED        conf=1.00  (21.3s)
  ├─ coordinator:   REJECTED        conf=1.00  (35.9s)
  Verification: override
  → MANUAL_REVIEW  conf=1.00  35.894s  [v1.0]  (expected: REJECTED)

  Appeal: ad_001
    ├─ appeal:   tool_call:check_landing_page
    ├─ appeal:   tool_call:query_policy_kb
    ├─ appeal:   tool_call:lookup_history
    ├─ appeal:   PARTIAL  conf=1.00  (15.3s)
    ├─ reasoning: The advertiser's appeal provides no specific evidence...

=== Scheduled Recheck ===
  Recheck: ad_002 (was PASSED, 24h recheck)
  ├─ coordinator:   directing review...
  ...
  → PASSED  conf=0.95  28.117s  (no change)

=== Monitor Report ===
  Reviews: 3 | Rejection rate: 33% | Avg confidence: 0.93 | Override rate: 100%
  Perception checks:
    ✓ Rejection spike      33% (threshold: 50%) — normal
    ⚠ Override rate        100% (threshold: 20%) — sample too small (1 verification)
    ✓ Advertiser cluster   no repeat offenders detected
    ✓ Policy hotspot       POL_002 (1 hits) — below threshold
    ✓ Confidence drop      0.93 (threshold: 0.70) — normal

=== Training Data Pool (标检训 → 训) ===
  3 samples from 2 sources:
    review                   2  — high-confidence review (≥0.9)
    verification_override    1  — verifier disagreed with REJECTED
    appeal_overturn          0
    active_learning          0
  Records:
    ├─ ad_001  source=review                   conf=0.98
    ├─ ad_001  source=verification_override    conf=0.98 (REJECTED → MANUAL_REVIEW)
    ├─ ad_002  source=review                   conf=0.95

=== Feature Showcase ===
  ✓ JSONL Persistence     9 reviews persisted (crash-safe, append-only)
  ✓ Model Routing         per-pipeline×role routing + 529 cross-provider fallback
  ✓ Scheduled Recheck     0 pending, 1 completed
  ✓ Training Data Pool    3 samples (review=2, verification_override=1, appeal_overturn=0, active_learning=0)
  ✓ Agent Memory          17 entries (policy=2, content=2, coordinator=3, single=4, region=6)
  Total Cost: $0.0458
```

## 技术栈

- Go 1.22+，零外部依赖（仅标准库）
- OpenAI 兼容 LLM API（已验证 xAI Grok）
- JSONL 追加写入持久化（crash-safe，WAL 模式——同一广告 ID 的重复条目属正常，恢复时取最新）
