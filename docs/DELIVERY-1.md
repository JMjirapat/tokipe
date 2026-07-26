# Delivery 1 — agentkit v1.0.0

**Date:** 2026-07-26
**Scope:** the complete BRD/tech-spec in [spec.md](spec.md), Phases 0–4
**Status:** released as `v1.0.0`
**Not yet:** pushed to a remote, or run against a real Anthropic endpoint

**Tagged on the owner's decision, not on a QA sign-off.** QA-REPORT.md §9's two
GO criteria are both met and the full verification suite passes, but QA had not
issued a round 6 verdict when the tag was cut. Recorded here so nobody later
reads the tag as evidence of a sign-off that did not happen.

Two consequences of tagging now, both accepted deliberately:

- **The interfaces in README §Stability are frozen as of this tag**, and they
  have only ever been exercised by this repository's own examples and tests.
  Spec §1.2's reusability criterion — adoption by two unrelated systems — is
  still unproven. If real integration shows an interface is wrong, that is a
  v2, per the freeze policy.
- **The module path is still `agentkit`**, so this tag is not `go get`-able
  from anywhere. It is a local milestone. Renaming to
  `github.com/<org>/agentkit` later means re-tagging v1.0.0 on the new path,
  because a different path is a different module.

## 0. QA history

### Round 5 — revision `044ccda`

Every behavioural finding from rounds 1-4 passes. What remained was the test
inventory in §2 having gone stale again — the same Blocker as round 1, for the
same reason, two rounds after "fixing" it by typing a fresh number.

| # | Finding | Resolution |
|---|---|---|
| R5-1 | Delivery §2 test inventory stale (297/222 claimed, 306/227 actual) | Fixed by making it machine-checked, not by retyping it. |
| R5-2 (Note) | Matrix matcher fell back to a bare method-name substring | Tightened to exact canonical identifiers. |

**R5-1 is the same mistake twice, so the number was not the problem.** A
hand-maintained count cannot survive rounds of new regression tests; every fix
that adds a test invalidates it. `inventory_test.go` now parses machine-readable
markers out of §2 and fails the build on drift. It proved itself immediately:
adding the guard added a test function, and the guard caught its own arrival
(227 → 228). The run-event total is deliberately left unpinned — checking it
would mean running the suite from inside the suite — so §2 publishes the
command instead of a number.

**R5-2 was a Note, and it was right.** The matcher accepted any point
containing the method name, so a point mentioning `Name` satisfied
`Stage.Name`, `ModelClient.Name` and `Rule.Name` alike; deleting the
`pipeline.Stage.Name` case would still have passed. It now matches exact
`pkg.Interface.Method` identifiers, and separately asserts that every point
*is* a known method so a typo fails loudly rather than silently covering
nothing. Tightening it immediately exposed a malformed point name of my own
(`"metrics.Recorder returning a nil Counter"`), and a negative test confirms
that removing the `Stage.Name` case now fails where it previously passed.

The pattern across five rounds: rounds 1-4 were behavioural, and each sweep
found more instances than were reported. Rounds 4 and 5 were about claims —
the matrix's completeness claim, then §2's inventory claim — and both were
false. Every claim in this document that can be machine-checked now is.

### Round 4 — revision `e4db7c7`

The sharpest round. QA did not look for another panicking dependency — it
tested the *completeness claim* the round 3 fix had just made, and the claim
was false. `extension_matrix_test.go` said it enumerated every method agentkit
calls on caller-supplied code, and it had omitted `metrics.Recorder` and
`metrics.Counter` entirely.

| # | Finding | Resolution |
|---|---|---|
| R4-1 | A panicking metrics backend broke the turn, discarding an already-computed result | Fixed at `metrics.Or`, which now returns a guarding decorator. |

Metrics are documented as opt-in and "never a hard dependency". They were a
hard dependency the moment they were configured: a panic from `Counter` or
`Inc` escaped `Pipeline.Run`, and for a successful preprocess rule the answer
had *already been computed* and was thrown away while incrementing its counter.
A nil `Counter` returned by a caller's backend crashed the same way.

The guard sits in `metrics.Or` rather than only in `metrics.Inc`, because two
stages call `rec.Counter(n).Inc(l)` directly. Wrapping at the point where a
caller's recorder enters the system covers every call style without each site
having to remember. `router.WithMetrics` was also assigning the raw recorder
without going through `Or` — found by sweep, unreported.

**What changed beyond the fix.** A prose claim about coverage is worth nothing,
which round 4 proved. `TestMatrixCoversEveryExtensionMethod` now reflects over
every extension interface and fails if any method has no matrix case. It was
negative-tested: renaming the metrics cases away makes it fail with the exact
missing method names. Adding a method to any extension interface now breaks the
build until the matrix covers it. Adding a whole new *interface* still needs a
line in that list — the one gap reflection cannot close, and it is documented
as such rather than claimed away.

`metrics` also had no test file at all while holding real logic; it is now at
96.7%.

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

These figures are **frozen as of tag `v1.0.0` (`d6f6429`)**. They are a
historical record of what shipped, not a description of the current tree — the
tree has moved on (Phase 5 and beyond), and a delivery record that silently
tracked HEAD would stop being evidence of anything.

The *live* inventory is machine-checked instead, in README §Verify the checkout.
That is where `inventory_test.go` reads its markers. Two rounds of QA Blockers
came from a hand-maintained count going stale; the lesson was that such a claim
must be enforced, and the corollary — learned here, when the guard failed the
build after Phase 5 — is that it must be enforced against a document that is
*supposed* to describe the present.

At `v1.0.0`:

| Quantity | Value | Command |
|---|---|---|
| Test/Example functions | 228 | `grep -rhoE '^func (Test\|Example)[A-Za-z0-9_]*' --include='*_test.go' . \| wc -l` |
| Packages in the root module | 25 | `go list ./... \| wc -l` |
| Failures | 0 | `go test -race -count=1 ./...` |

| Claim | Evidence | Result |
|---|---|---|
| Everything builds and passes under the race detector | `go test -race ./...` | 0 failures across 25 packages; inventory above |
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
| `metrics` | 96.7% | | | |

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

- [x] Independent verification and QA rounds 1-5 — see [QA-REPORT.md](QA-REPORT.md)
- [x] Tag `v1.0.0` — cut on the owner's decision; interfaces now frozen
- [ ] QA round 6: re-verify against the GO criteria in QA-REPORT.md §9. If it
      finds a Blocker, that is a `v1.0.1`, not an amended tag
- [ ] Decide the final module path and re-tag on it
- [ ] Decide the final module path (`agentkit` → `github.com/<org>/agentkit`)
- [ ] Run the Anthropic prompt-caching test once credit exists
- [ ] Set a git remote and push
