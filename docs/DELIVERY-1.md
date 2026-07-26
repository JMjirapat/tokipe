# Delivery 1 — agentkit v1.0.0-rc1

**Date:** 2026-07-26
**Scope:** the complete BRD/tech-spec in [spec.md](spec.md), Phases 0–4
**Status:** QA rounds 1-3 complete; all findings resolved, awaiting re-verification
**Not yet:** tagged, pushed, or run against a real Anthropic endpoint

## 0. QA history

### Round 3 — revision `03acda9`

All nine earlier inputs pass. QA found one Minor, in two parts, and it is the
most useful finding of the three rounds because it caught a claim of *mine*
rather than a defect: Delivery round 2 said the sweep had covered every
caller-supplied `Name()` on the request path. It had not.

| # | Finding | Resolution |
|---|---|---|
| R3-a | Docs said a custom-stage panic is returned as `StageError`; it actually propagates | Fixed the docs — the behaviour was right, the description was not. |
| R3-b | `Stage.Name()` was unguarded while building `StageError` | Fixed. Also found `Router.Route` unguarded — unreported. |

R3-a was a documentation defect, not a behaviour one. Not recovering a
caller's own `Stage.Process` panic is deliberate; the docs simply listed it
among the returned errors. Both `agentkit.go` and the README now state plainly
that it propagates and that recovering it is the caller's job.

R3-b mattered more than its severity suggests: a broken `Name()` could replace
the real error `Process` had already returned, destroying the only useful
signal in favour of a label. The sweep it prompted also found a panicking
`Router.Route` escaping — routing is an optimization, so it now falls back to
the default client and records `router.reason = "router_panicked"`.

**The pattern, and what closed it.** Each round reported one unguarded
extension point, and each sweep found two or three more of the same class:

```
round 1: tool Executor  → also Rule, Compressor, Embedder, VectorStore
round 2: Rule.Name      → also ModelClient.Name, in router and in pipeline
round 3: Stage.Name     → also Router.Route
```

Fixing instances one at a time was losing to that pattern, so
`extension_matrix_test.go` now enumerates every method agentkit calls on
caller-supplied code — 14 cases — and asserts each one's contract. A new
extension point has to be added to that table before it can be forgotten.

### Round 2 — revision `b5cdd38`

QA re-verified the six fixes at their original inputs (all pass) and then went
looking for *adjacent* cases the new boundaries missed. It found three more, and
returned NO-GO again. Correctly: two were real gaps in the round 1 fix rather
than new code.

| # | Finding | Resolution |
|---|---|---|
| R2-1 | `Rule.Name()` still outside the panic boundary | Fixed. **Wider than reported** — see below. |
| R2-2 | A `Cache.Get` panic suppressed the tool execution entirely | Fixed: `Get`, `Executor` and `Set` now have separate boundaries. |
| R2-3 | Empty `Query` still left retrieved context after the newest message | Fixed: the newest turn is derived from `Messages`, not from `Query`. |

**R2-1 was wider than reported, in the same way round 1 was.** QA found the
unguarded `Name()` on preprocess `Rule`. Sweeping every call into a
caller-supplied `Name()` found the same hole on `ModelClient` inside both the
router's metrics and `Pipeline.Run`'s routing metadata. All now go through
`safe.Name`, which falls back to a placeholder. `Name()` looks too trivial to
fail, which is exactly why it was missed twice — and a panicking `Name` was
discarding results that had *already succeeded*.

`Registry.Register` still calls `Name()` unguarded, deliberately: that runs at
wiring time, not on the request path, and failing fast on a broken rule during
construction is the desired behaviour.

**R2-2 is the sharper lesson.** The round 1 fix wrapped the whole
get→execute→store sequence in one boundary, which made a panic behave
*differently* from an error at the same step: an ordinary `Get` error degrades
to a miss and the tool still runs, but a `Get` panic aborted everything and the
tool never ran. Containment is not enough — a contained panic has to land in
the same place the equivalent error does. Each step now has its own boundary.

### Round 1 — revision `94277a1`

[QA-REPORT.md](QA-REPORT.md) returned **NO-GO** with 2 Blockers, 3 Majors and
1 Minor. Every finding was independently reproduced before being fixed — none
was taken on trust, and none was disputed: all six were real.

| # | Finding | Resolution |
|---|---|---|
| 1 | Tool executor panic escaped `Pipeline.Run` | Fixed at `internal/safe`. **Wider than reported** — see below. |
| 2 | Test count not reproducible | Fixed: counts now quoted with the command that produces them. |
| 3 | Non-UTF-8 args collided in `HashToolCall` | Fixed: invalid UTF-8 rejected as `ErrUncacheable` before marshalling. |
| 4 | Anthropic breakpoint missed static content | Fixed: `cache_control` now placed against final wire order. |
| 5 | Retrieved context could follow the newest message | Fixed: the newest turn is held back and appended exactly once. |
| 6 | Docs overstated which errors `Run` returns | Fixed in `agentkit.go` and README. |

**Finding 1 was worse than reported.** QA found the panic escape in the tool
Executor. Probing the other caller-supplied extension points showed the same
defect in preprocess `Rule`s, `Compressor`s, and the `Embedder`/`VectorStore`:
one bug in four places, each of which broke the fail-open guarantee. All four
now run behind `internal/safe`, and a panic is contained exactly like an error.

Caller-supplied `pipeline.Stage`s added via `config.WithStage` are deliberately
*not* wrapped. That is the caller's own code in the caller's own pipeline;
swallowing its panics would hide their bugs rather than tolerate a third
party's. This is now stated in the `agentkit` package documentation.

On severity: finding 2 was filed as a Blocker under the brief's rule that a
false claim in §2 is a Blocker. The number was imprecise rather than false —
all tests passed and the package count was right — so Minor would have been
fairer. It is fixed either way, and the rule that produced the call is a good
rule; the note is recorded only so the severity distribution is not misread.

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

Test counts are quoted with the command that produces them, because "237
tests" turned out not to be reproducible with any plain `go` invocation — it
came from a wrapper's summary line. Counting method, not just the number:

```bash
go test -race -json -count=1 ./... | grep -c '"Action":"run"'   # 293, incl. subtests
grep -rhoE '^func (Test|Example)[A-Za-z0-9_]*' --include='*_test.go' . | wc -l  # 221 funcs
go list ./... | wc -l                                            # 25 packages
```

| Claim | Evidence | Result |
|---|---|---|
| Everything builds and passes under the race detector | `go test -race ./...` | 293 run events / 221 test funcs, 25 packages, 0 failures |
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
| `budget` | 100.0% | | `rag` | 97.7% |
| `router` | 100.0% | | `cache` | 97.2% |
| `agentkit` | 100.0% | | `providers/cli` | 94.3% |
| `compress` | 98.9% | | `providers/anthropic` | 94.6% |
| `preprocess` | 98.8% | | `toolcache` | 93.7% |
| `pipeline` | 94.4% | | `config` | 90.5% |
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

- [x] Independent verification and QA rounds 1-3 — see [QA-REPORT.md](QA-REPORT.md)
- [ ] QA round 4: re-verify the round 3 fixes against the GO criteria in
      QA-REPORT.md §7
- [ ] Decide the final module path (`agentkit` → `github.com/<org>/agentkit`)
- [ ] Run the Anthropic prompt-caching test once credit exists
- [ ] Set a git remote and push
- [ ] Tag `v1.0.0` — after which the interfaces in README §Stability are frozen
