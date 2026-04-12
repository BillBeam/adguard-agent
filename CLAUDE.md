# AdGuard Agent

## 交互语言

所有回答内容和思考过程使用中文。

## Project Overview

AdGuard Agent is a Multi-Agent content safety system for international ad review, built in Go. The project has two parallel development tracks:

- **Business Track**: Ad review domain logic aligned with ByteDance's "Universal Strategy Platform + Orchestration Engine" — data-driven strategy matrix, perception-attribution-adjudication-governance pipeline, label-check-train loop, false-positive control, appeal workflow.
- **Technical Track**: Production-grade AI Agent design patterns — message type system, API client factory, retry with backoff, usage tracking, tool system, agentic loop state machine.

## Architecture

```
cmd/adguard/         — CLI entry point
internal/types/      — Shared type definitions (messages, review domain, strategy)
internal/llm/        — LLM client (OpenAI-compatible, multi-provider)
internal/config/     — Three-tier config (env > file > defaults)
internal/strategy/   — Strategy matrix (policy × region → review plan)
data/                — Policy KB, region rules, category risk, test samples
```

## Go Conventions

- Go 1.22+, minimal third-party dependencies (stdlib only where possible)
- Error handling: `fmt.Errorf("context: %w", err)` — always wrap with context
- Logging: `log/slog` structured logger
- Tests: table-driven, `go test ./...`
- JSON: all domain types carry `json:"..."` tags

## Design Reference

实现新模块时，仍然必须先研读 `_ref/src/` 中对应的源码，理解设计原理，然后用 Go 惯用方式实现等价系统。`_ref/src/` 是设计的金标准，这一点不变。

## Comment Style

源码注释只写设计意图和技术决策的解释，不写设计来源或参考架构引用。注释应该解释"为什么这样设计"，而不是"这个设计从哪里来"。不在注释中出现 `.ts`/`.tsx` 文件名、行号、外部常量名、"reference architecture"、"production agent" 等措辞。

## Plan Mode

当用户明确开启 plan mode 时，必须先写 plan 文件确认理解对齐，经用户确认后再执行。即使用户给的任务描述已经很详细，也不能跳过 plan 直接实现。

## Testing Discipline

测试失败时，必须先判断是**项目代码逻辑的问题**还是**测试代码的问题**，是哪里的问题就修哪里。不能为了通过测试而随意修改项目代码逻辑，也不能为了通过测试而降低测试的真实性。

## Real LLM Test Data

每次使用真实 API key 运行 demo 后，必须将测试数据和终端输出保存到 `data/real-llm-runs/` 并推送到 GitHub：

1. 运行：`LLM_API_KEY=xxx go run ./cmd/adguard/ 2>&1 | tee data/real-llm-runs/run-output.txt`
2. 复制 JSONL：`cp data/{reviews,training,appeals,rechecks}.jsonl data/real-llm-runs/`
3. **检查无 API key 泄漏**：`grep -r "xai-\|api_key\|API_KEY" data/real-llm-runs/`（必须为 0）
4. 提交并推送

`data/*.jsonl` 被 `.gitignore` 排除（运行时数据），`data/real-llm-runs/` 是版本控制的测试证据。

## Key Commands

```bash
go build ./...                      # Build all packages
go vet ./...                        # Static analysis
go test ./internal/strategy/... -v  # Run strategy matrix tests
go run ./cmd/adguard/               # Run CLI (mock mode)
LLM_API_KEY=xxx go run ./cmd/adguard/  # Run CLI (real LLM mode)
```
