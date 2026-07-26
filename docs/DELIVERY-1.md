# Delivery 1 — agentkit v1.0.0-rc1

**Date:** 2026-07-26
**Scope:** the complete BRD/tech-spec in [spec.md](spec.md), Phases 0–4
**Status:** feature-complete, ready for independent verification
**Not yet:** tagged, pushed, or run against a real Anthropic endpoint

---

## 1. What is being delivered

A Go library that reduces LLM input-token cost by composing optimization
stages between a caller and a model provider. It is a library, not a service:
no hosted component, no UI, no ML model, no required provider.

| Deliverable | Location |
|---|---|
| Core pipeline and stages | `pipeline/`, `preprocess/`, `toolcache/`, `rag/`, `compress/`, `lazyload/`, `cache/`, `router/`, `budget/`, `metrics/` |
| Assembly surface | `config/`, `agentkit.go` |
| Backends | `providers/anthropic` (API key), `providers/cli` (no API key) |
| Retrieval adapters | `stores/mock`, `stores/pgvector` |
| Cache backends | `toolcache` in-memory, `toolcache/redis` |
| Runnable examples | `examples/rag-chatbot`, `examples/local-routing`, `examples/coding-agent`, `examples/cli-provider` |
| Benchmark | `benchmarks/` |
| CI | `.github/workflows/ci.yml` |

## 2. Evidence of completion

Reproduce all of this from a clean checkout:

```bash
go build ./... && go vet ./... && go test -race ./...
```

| Claim | Evidence | Result |
|---|---|---|
| Everything builds and passes under the race detector | `go test -race ./...` | 237 tests, 24 packages |
| No CGo required | `CGO_ENABLED=0 go build ./...` | clean |
| Core has zero third-party dependencies | `go list -deps ./...` filtered, enforced in CI | none |
| ≥30% input-token reduction | `go run ./benchmarks` | **57.1%** |
| Requests short-circuited without an LLM | same benchmark | 3/12 turns |
| Two-tier routing with no per-request routing code | `examples/local-routing`, `examples/cli-provider` | works, mocks and real models |
| Concurrency safety of shared stateful components | `TestConcurrentRunsShareStatefulComponents` | 480 concurrent runs, `-race` clean |
| No stampede on concurrent identical tool calls | same test | exactly 2 executions for 2 distinct calls |
| CLI backends work without an API key | `AGENTKIT_CLI_LIVE=1 go test -run TestLiveCLIs ./providers/cli/` | claude, codex, opencode all return |

Coverage, all above the spec's ≥80% bar for the five named packages:

| Package | Coverage | | Package | Coverage |
|---|---|---|---|---|
| `budget` | 100.0% | | `rag` | 97.6% |
| `router` | 100.0% | | `cache` | 97.2% |
| `agentkit` | 100.0% | | `providers/cli` | 94.3% |
| `compress` | 98.9% | | `providers/anthropic` | 94.3% |
| `preprocess` | 98.8% | | `toolcache` | 93.0% |
| `pipeline` | 97.0% | | `config` | 90.5% |
| `stores/mock` | 96.7% | | `lazyload` | 88.1% |

## 3. What is NOT verified

Read this before signing off. These are known, deliberate, and documented —
they are not defects to rediscover.

| Gap | Why | Risk if wrong |
|---|---|---|
| **Anthropic prompt caching against the real endpoint** | Needs pay-as-you-go API credit, which the project owner does not have. A Claude Pro/Max subscription is a different product and issues no API key. Test is written and skips cleanly. | The headline caching benefit is designed and unit-tested, **not field-proven**. Do not quote a cache-hit rate externally. |
| **`stores/pgvector` against a real database** | Integration test is gated on `AGENTKIT_PGVECTOR_DSN`. | SQL correctness beyond identifier validation is unproven. |
| **`compress.CodeCompressor`** | Deliberate stub; `CanHandle` always returns false. AST-aware compression is Phase 2+ per spec §2.4.4. | None today — it is inert by construction. |
| **Benchmark token accounting** | Estimates 4 chars/token rather than using a real tokenizer. Both arms measured identically, so the *ratio* holds. | The 57.1% figure is sound as a comparison, weaker as an absolute. |
| **Real-world integration** | Spec §1.2 wants adoption by two unrelated systems. Currently proven by examples only. | Interface gaps that only appear under real use. |

## 4. Documented deviations from the spec

All four are recorded in [../PLAN.md](../PLAN.md) with rationale. Summarised so
a reviewer comparing spec to code is not surprised:

1. `ModelClient` and `Router` live in `pipeline`, not `providers`/`router` —
   the spec's placement is an import cycle. Both are re-exported as type
   aliases, so every signature in the spec still compiles for callers.
2. `pipeline.Message` gained a `Static bool` field — §2.4.6 requires
   "caller-marked" static content but the spec's struct had no field for it.
3. `toolcache.Stage` is new — the spec defines only the passive `Cache`
   interface, while the §2.1 diagram shows ToolCache as a pipeline stage.
4. Constructors needing options gained a second entry point (`NewStageWith`,
   `NewHeuristicRouterWith`) because Go cannot take two variadic parameters.
   The spec's signatures are unchanged.

Plus two decisions made where the spec left the choice open:
`providers/anthropic` stays in the core module (it is `net/http`-only, so the
zero-dependency guarantee holds), and `providers/cli` was added beyond scope
because the owner has no API credit.

## 5. Handover checklist for the next phase

- [ ] Independent verification and QA — see [QA-BRIEF.md](QA-BRIEF.md)
- [ ] Decide the final module path (`agentkit` → `github.com/<org>/agentkit`)
- [ ] Run the Anthropic prompt-caching test once credit exists
- [ ] Set a git remote and push
- [ ] Tag `v1.0.0` — after which the interfaces in README §Stability are frozen
