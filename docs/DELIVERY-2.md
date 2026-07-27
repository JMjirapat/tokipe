# Delivery 2 — tokipe, post-v1.0.0 work

**Date:** 2026-07-27
**Scope:** ROADMAP Phases 5–8, plus the module-path rename
**Status:** QA Round 4 **GO** for the current working tree
**Range:** `v1.0.0-agentkit-path..HEAD`
**Not yet:** pushed to a remote, or run against a real Anthropic endpoint

Delivery 1 ([DELIVERY-1.md](DELIVERY-1.md)) covered the original spec, Phases
0–4, and is frozen. This document covers everything since. Read them in order;
this one does not repeat what that one established.

## 0. QA round 1 — revision `093fd6c`

[QA-REPORT-2.md](QA-REPORT-2.md) returned **NO-GO** with 2 Blockers and 7
Majors. Every finding was reproduced independently before being fixed; all nine
were real, and none was disputed.

| # | Finding | Resolution |
|---|---|---|
| B1 | The CI dependency guard still excluded the *old* module path, so it flagged every internal package and would fail the build | Pattern updated; the exact CI command now passes |
| B2 | A `TokenCounter` panic escaped `Pipeline.Run` during trimming | Every counter call is behind the boundary; both missed extension points registered |
| M1 | A duplicated current turn was charged twice, dropping history the real request had room for | `CountRequest` charges `Query` only when it is not already the newest message |
| M2 | An over-budget request with no droppable message never reached the chunk pass | Chunk trimming now runs regardless |
| M3 | Abandoning a CLI stream waited for the child before cancelling it | Cancel first, then reap |
| M4 | Ordinary cache `Get` errors were invisible — the exact case Phase 7 existed for | Reported, plus four other silent seams found by the same audit |
| M5 | `CodeCompressor` stripped `//go:build`, `//go:embed` and cgo preambles | Directive-bearing files are refused outright |
| M6 | Dedupe discarded a near-copy differing in one decisive value | Threshold raised to 0.95 and a value-agreement guard added |
| M7 | `metrics/otel` declared `go 1.25.0`, contradicting the documented 1.23 minimum | OTel pinned to 1.31.0; verified with `GOTOOLCHAIN=local` |

**Three of these are failures of my own verification, not just of the code.**

*B1* is the sharpest. I checked that claim by hand with a corrected grep, not
with the command CI actually runs — so my check passed while the job would have
failed on the first push. Verifying a claim with a different command than the
one documented is not verification.

*B2* got past the extension matrix because the matrix only checks interfaces it
has been told about, and Phase 6 added two — `budget.TokenCounter` and
`history.Summarizer` — that nobody registered. The guard reported completeness
over a list that was incomplete. Both are now in it.

*M7* happened because `go mod tidy` raised both the OTel version and the `go`
directive, and I never read the result. It passed locally only because
`GOTOOLCHAIN=auto` silently downloaded Go 1.25.

**A regression test that could not fail.** The first `budget.TokenCounter`
matrix case used a fixed 64-call grace period before panicking; trimming
finished inside it, so the case passed *with the fix reverted*. It now measures
the guarded call count for the request and panics on the first call after it.
Every fix in this round was negative-tested the same way — reverted, confirmed
failing, restored.

## 0.1 QA Round 2 implementor follow-up submitted for re-verification

Round 2 found one stale-evidence Blocker and three remaining Major defects.
The implementor submitted these four remediations:

| Finding | Implementation |
|---|---|
| Coverage evidence was stale | Recomputed after the Round 2 code and tests; Delivery and README reported 393 test functions and that checkpoint's package percentages |
| A shell descendant could retain stream pipes after cancellation | CLI subprocesses use their own process group on Unix, abandoned streams terminate the group and close stdout before reaping, and `WaitDelay` bounds inherited stdout/stderr pipes |
| Ordinary C code in a cgo preamble was not recognized | `CodeCompressor` refuses every AST containing `import "C"` rather than guessing from comment text |
| `allow` versus `deny` could still be deduplicated | The default threshold is now 1.0 (normalized exact match); lossy near-deduplication requires an explicit lower threshold |

The three Round 2 external reproductions pass, including the shell process-tree
case. Root and nested race suites, benchmarks, all examples, the exact CI
dependency command, and a Windows compile check also pass. This is implementor
verification, not independent QA sign-off.

QA Round 3 verified the coverage, CLI process-tree, and cgo fixes. It rejected
the dedupe fix: Jaccard 1.0 over a set of shingles is not an exact normalized
sequence comparison, so distinct periodic content can still be discarded. See
[QA-REPORT-2.md §4](QA-REPORT-2.md#4-qa-round-3-re-verification).

The implementor response now compares the complete normalized word sequence
when the threshold is 1.0. Jaccard is used only for explicitly configured
thresholds below 1. The Round 3 reproduction and all earlier dedupe tests pass;
QA Round 4 verified the fix and issued GO for the current working tree.

---

## 1. What is being delivered

| Phase | Deliverable | Packages |
|---|---|---|
| 5 | Streaming — incremental results without touching the frozen `ModelClient` | `pipeline`, `providers/anthropic`, `providers/cli`, `providers/mock` |
| 6 | Context budget enforcement — the thing `budget.Policy` computed but nothing consumed | `history`, `budget` |
| 7 | Observability — making fail-open visible instead of silent | `metrics`, `metrics/otel` (new nested module) |
| 8 | AST code compression, chunk dedupe, pgvector CI, OpenAI-compatible provider | `compress`, `providers/openai` |
| — | Module path is now `github.com/JMjirapat/tokipe` | everything |

New public surface, all additive — nothing in the v1.0.0 freeze changed:

```go
pipeline.StreamingClient, pipeline.Delta, Pipeline.RunStream, Collect, StreamOne
budget.TokenCounter, CharEstimator, CountRequest, RequestCost
history.Stage, Summarizer, ElisionSummarizer
metrics.HistogramRecorder, GaugeRecorder, DegradationReporter, Degradation, Timed
compress.CodeCompressor (was a no-op stub), compress.DedupeStage
providers/openai.Client
config.WithHistoryBudget, WithChunkDedupe
```

## 2. Evidence

Reproduce from a clean checkout:

```bash
go build ./... && go vet ./... && go test -race -count=1 ./...
CGO_ENABLED=0 go build ./...
```

| Claim | Command | Result |
|---|---|---|
| Root module builds, vets and passes under `-race` | `go test -race -count=1 ./...` | 0 failures, 29 packages, 394 test functions |
| No CGo required | `CGO_ENABLED=0 go build ./...` | clean |
| Core still has zero third-party dependencies | the exact CI command, re-run verbatim | none |
| Three nested modules build and pass, on Go 1.23 | `cd <mod> && GOTOOLCHAIN=local go test -race ./...` | pgvector 14, redis 7, otel 7 |
| v1 token reduction unchanged by any of this | `go run ./benchmarks` | **57.1%** (target ≥30%) |
| Phase 6 saving, measured separately | same, long-loop section | **54.6%** over 100 turns, peak 1192 vs a 1200 budget |
| Streaming works against real CLIs | `AGENTKIT_CLI_LIVE=1 go test -run TestLiveCLIStreaming ./providers/cli/` | claude, codex, opencode all stream |
| Fail-open survives every dependency failing | `go run ./examples/observability -break` | 40/40 turns succeed, 80 degradations logged |
| Six examples run end to end | `go run ./examples/<name>` | all pass, no credentials |

Coverage after this delivery:

| Package | Cov | | Package | Cov |
|---|---|---|---|---|
| `router` | 100.0% | | `history` | 94.4% |
| `agentkit` (root) | 100.0% | | `providers/anthropic` | 93.2% |
| `internal/safe` | 100.0% | | `toolcache` | 93.2% |
| `preprocess` | 96.7% | | `metrics` | 91.1% |
| `rag` | 97.9% | | `providers/cli` | 90.0% |
| `cache` | 97.2% | | `pipeline` | 90.4% |
| `compress` | 93.3% | | `config` | 89.7% |
| `budget` | 96.1% | | `providers/openai` | 88.9% |
| `stores/mock` | 96.7% | | `lazyload` | 88.1% |

Two packages were below standard when this document was first drafted and were
fixed before handover rather than disclosed as gaps: `budget` (41.3% — the
Phase 6 counter had no direct tests) and `internal/safe` (0% — the fail-open
primitive itself, exercised only indirectly through the extension matrix).

## 3. What is NOT verified

Read this before signing off. These are known and deliberate.

| Gap | Why | Risk |
|---|---|---|
| **Anthropic prompt caching, real endpoint** | Needs pay-as-you-go API credit; a Claude Pro/Max subscription is a different product and issues no API key. Test written, skips cleanly. | The caching benefit is designed and unit-tested, **not field-proven**. Unchanged since Delivery 1. |
| **pgvector against a real database** | A CI job now exists and fails if its tests skip, but it has never run — nothing has been pushed. No Docker on the development machine. | SQL correctness beyond identifier validation is still unproven *in practice*. |
| **OpenAI provider against a real server** | No API key available. Everything is `httptest`-based. | Wire-format assumptions are unconfirmed against any live OpenAI-compatible server. |
| **Streaming under adverse networks** | Tested against `httptest` and real CLIs, never against a slow, lossy or half-closed connection. | Resource-cleanup paths are the risk; see QA-BRIEF §B. |
| **Real-world integration** | Spec §1.2 wants two unrelated adopting systems. Still only this repository's examples. | Interface gaps that appear only under real use. |
| **Benchmark token accounting** | 4 chars/token estimate, not a tokenizer. Both arms measured identically, so the *ratio* holds. | Sound as comparison, weaker as an absolute. |

## 4. Decisions worth reviewing

Each of these is a judgement call a reviewer may reasonably disagree with.

1. **Streaming is an optional interface, not a change to `ModelClient`.**
   `ModelClient` froze at v1.0.0. Widening it would break every implementation.
   `Run` and `RunStream` share one `prepare` helper so the two paths cannot
   drift.

2. **History trimming removes the MIDDLE, not the oldest turns.** Dropping from
   the front changes the first non-static bytes every turn, so providers that
   cache on longest-common-prefix never hit. Head and tail survive.

3. **The degradation sink lives on `metrics.Recorder`.** One object, one wiring
   point. `Degradation.Err` deliberately never becomes a metric attribute —
   unbounded strings are how a backend gets a cardinality explosion.

4. **"Semantic dedup" was delivered as *lexical* dedup and renamed.** Shingled
   Jaccard catches copies, not paraphrases. Shipping it under "semantic" would
   have claimed something the code does not do.

5. **`CodeCompressor` replaced a no-op stub.** The signature is unchanged so
   nothing fails to compile, but a registered compressor now actually claims Go
   chunks. Callers who registered it to pin ordering — as the stub's own docs
   suggested — will see behaviour change.

6. **Bedrock was dropped from Phase 8 rather than half-built.** SigV4 needs the
   AWS SDK, which the stdlib-only core cannot take.

7. **The module path is `.../tokipe` while the root package is `agentkit`.**
   Legal Go, mildly surprising. Renaming the package touches every example and
   doc; recorded in PLAN.md as a separate open decision.

8. **The `v1.0.0` tag was moved.** The original pointed at a commit declaring
   `module agentkit` — a path that no longer exists and would resolve for
   nobody. It is preserved as `v1.0.0-agentkit-path`; `v1.0.0` now marks the
   first release on the real path. Nothing was ever published, so no consumer
   was affected.

## 5. Bugs found and fixed during this delivery

Listed because they show where the risk actually was, and because several were
found by tests written specifically to doubt a claim rather than to confirm it.

| Phase | Bug | How it was caught |
|---|---|---|
| 5 | `cmd.Wait()` called twice in CLI stream cleanup, masking the real exit status | Reading the cleanup path back |
| 5 | Anthropic usage overwritten instead of merged — a later SSE frame zeroed an earlier one | Test asserting input *and* output tokens together |
| 6 | A configured summariser could never fire: trimming left no headroom, so the summary was always rejected | Its own test failing |
| 6 | The stage returned a copy, so the caller's pointer never saw the trim — the benchmark measured an untrimmed request while billing a trimmed one | Benchmark reporting 0 dropped while tokens fell 55% |
| 6 | Benchmark arms briefly stopped recording identical content, inflating the headline 57.1% → 67.4% | Noticing the number improved for no reason |
| 7 | `metrics.Or`'s safety wrapper hid every new optional interface, silently discarding all histograms, gauges and degradations | A test written to check exactly that |
| 8 | Test-file string literals mangled by an escaping error during authoring | Compiler |

## 6. Handover

- [x] Independent QA round 1 — see [QA-REPORT-2.md](QA-REPORT-2.md)
- [x] QA round 2 completed; implementor submitted four follow-up fixes
- [x] QA round 3 independent re-verification — **NO-GO**, one Major remains
- [x] Default dedupe now compares normalized sequences exactly
- [x] QA round 4 re-verification — **GO** for the current working tree
- [ ] Decide whether the root package should be renamed `tokipe`
- [ ] Push to `github.com/JMjirapat/tokipe` (remote configured, nothing pushed)
- [ ] Run the Anthropic prompt-caching test once API credit exists
- [ ] Let the pgvector CI job run for the first time
