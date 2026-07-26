# QA Verification Report — tokipe Delivery 2

**Review date:** 2026-07-27  
**Revision:** `093fd6c`  
**Scope:** [DELIVERY-2.md](DELIVERY-2.md), ROADMAP Phases 5–8 and the module-path rename  
**Procedure:** [QA-BRIEF.md](QA-BRIEF.md)  
**Baseline:** [DELIVERY-1.md](DELIVERY-1.md)  
**Current result:** **NO-GO for accepting Delivery 2**

## 1. Executive summary

The documented baseline is substantially healthy. The root module builds and
vets, passes its race suite across 29 packages, builds without CGo, and
reproduces both benchmark results: 57.1% on the v1 workload and 54.6% over the
100-turn history workload. All three nested modules pass their race suites, all
six examples complete, the broken-dependencies observability example keeps
40/40 turns alive, and every reported package coverage value is reproducible.

Independent adversarial testing nevertheless found two release blockers:

1. the CI check cited as enforcement for the zero-third-party-dependency claim
   was not updated for the module rename and fails on every internal package;
2. a caller-supplied `budget.TokenCounter` panic can escape `Pipeline.Run`
   during trimming, breaking the central fail-open guarantee.

Seven additional plausible-input defects affect history budgeting, streaming
cleanup, observability, code compression, chunk deduplication, and the stated
minimum Go version.

| Severity | Count |
|---|---:|
| Blocker | 2 |
| Major | 7 |
| Minor | 0 |
| Note | 0 |

The external reproductions were placed in `/tmp/tokipe-qa2`; no production
source code was changed during this review.

## 2. Blockers

### [Blocker] The CI enforcement cited by Delivery §2 fails after the module rename

**File:** `.github/workflows/ci.yml:38-45`  
**Claim:** `DELIVERY-2.md:50` says the root module has no third-party
dependencies and that this is enforced in CI.  
**Reality:** The dependency filter still excludes the old `agentkit` prefix.
Every package under the new `github.com/JMjirapat/tokipe` path therefore
matches the “third-party” pattern, leaves `deps` non-empty, and fails the job.
The underlying root module is still stdlib-only; the claimed enforcement is
what is broken.

**Reproduction:**

```bash
deps=$(go list -deps ./... \
  | grep -E '^[^/]+\.[^/]+/' | grep -v '^agentkit' || true)
printf '%s\n' "$deps"
test -z "$deps"
```

Observed: all 29 project packages are printed and `test -z` exits 1.

**Impact:** The core CI job will fail when the repository is pushed, and a
Delivery §2 evidence-table claim is false. The QA brief classifies a false
evidence claim as a Blocker.

---

### [Blocker] `TokenCounter` panic escapes while history is being trimmed

**File:** `history/stage.go:286-296`, also `342-344` and `373-382`  
**Claim:** `history.Stage` is fail-open, and caller-supplied token counters are
contained so a counter panic costs the trim rather than the turn.  
**Reality:** Only the initial `CountRequest` call goes through `safe.Value`.
Once a request is over budget, the stage calls `s.counter.CountTokens`
directly while dropping messages, sizing a summary, and trimming chunks. A
panic from any of those calls escapes `Pipeline.Run`.

The extension-matrix completeness guard does not include
`budget.TokenCounter` (or `history.Summarizer`), so it passes while this new
extension point is uncovered.

**Reproduction:**

```bash
cd /tmp/tokipe-qa2
go test -count=1 -run TestTokenCounterPanicDuringTrimCannotBreakTurn -v
```

Observed:

```text
caller-supplied TokenCounter panic escaped Pipeline.Run:
token counter panicked during trimming
```

**Impact:** A cost-control optimization can take down an otherwise valid model
turn. This directly breaks the load-bearing fail-open guarantee and is a
Blocker under the QA brief.

## 3. Major findings

### [Major] A duplicated current turn is counted twice and causes unnecessary history loss

**File:** `budget/counter.go:98-100`, `128-158`  
**Claim:** History budgeting fits the request that the provider will receive,
while preserving as much useful context as the budget allows.  
**Reality:** `CountRequest` always adds both `Request.Query` and the newest
message. Anthropic and OpenAI adapters deliberately send the current turn only
once when those two representations contain the same text. This common request
shape is therefore over-counted and can lose history even when the actual wire
request is already below budget.

**Reproduction:**

```bash
cd /tmp/tokipe-qa2
go test -count=1 -run TestDuplicatedCurrentTurnCausesUnnecessaryHistoryLoss -v
```

Observed:

```text
history was dropped even though the provider-visible request fits:
messages=2
history.tokens_before=31
history.tokens_after=21
```

The provider-visible estimate was approximately 21 tokens against a budget of
25; the stage counted 31 by charging the current turn twice.

**Impact:** Callers using the request shape shown by existing examples can
silently lose relevant conversation context and answer quality.

---

### [Major] An over-budget request with only protected messages never trims multiple chunks

**File:** `history/stage.go:270-275`, `320-325`  
**Claim:** When message trimming is insufficient, `history.Stage` trims
retrieved chunks from lowest similarity upward.  
**Reality:** `trim` returns immediately when there are no droppable messages,
before it reaches the chunk-trimming branch. A request containing only a static
prefix, the newest message, and multiple large retrieved chunks remains over
budget even though all but the best chunk are legal candidates.

**Reproduction:**

```bash
cd /tmp/tokipe-qa2
go test -count=1 -run TestChunkOnlyOverBudgetRequestStillTrimsChunks -v
```

Observed:

```text
over-budget request with two chunks kept both because no history message was
droppable: chunks=2
```

**Impact:** Budget enforcement fails on a normal one-turn RAG request and may
send an avoidably over-limit request to the provider.

---

### [Major] Abandoning a CLI stream waits for the child before cancelling it

**File:** `providers/cli/stream.go:86-107`, `124-127`  
**Claim:** Breaking out of a CLI stream cancels and reaps the subprocess rather
than leaving it running.  
**Reality:** The deferred cleanup calls `reap()` before `cancel()`. `reap`
drains stdout and waits for the child, so a consumer that breaks after the
first delta blocks until the child exits naturally or the configured timeout
fires. Cancellation comes too late to stop that wait.

**Reproduction:**

```bash
cd /tmp/tokipe-qa2
go test -count=1 -run TestAbandoningCLIStreamReturnsPromptly -v
```

The test streams one line, sleeps for two seconds, then streams another line.
Observed:

```text
breaking after the first delta blocked for 2.02s waiting for the child to exit
```

**Impact:** A client disconnect or UI cancellation can hold request goroutines
and subprocesses until the CLI exits or the default five-minute timeout
expires. Under load this becomes resource exhaustion.

---

### [Major] Ordinary cache `Get` errors remain invisible to observability

**File:** `toolcache/stage.go:166-186`  
**Claim:** Phase 7 makes fail-open degradation visible; the motivating example
is a dead cache backend silently degrading every request.  
**Reality:** `Cache.Get` returning an ordinary error is folded into the same
branch as a normal miss and the error is discarded. Only a panic reports
`cache_get_panicked`. The tool executes and the turn succeeds, but the
degradation sink receives no event.

**Reproduction:**

```bash
cd /tmp/tokipe-qa2
go test -count=1 -run TestOrdinaryCacheGetFailureIsObservable -v
```

Observed:

```text
cache Get returned an error but no degradation was reported
```

**Impact:** The most common cache-outage failure mode is indistinguishable from
healthy cache misses, defeating the production-observability goal.

This is not the only silent fail-open seam found by inspection:
`Rule.CanHandle` panics, `Compressor.CanHandle` panics, rejected/failing
summaries, and failures after an uncacheable tool key also lack a degradation
event.

---

### [Major] `CodeCompressor` strips semantic Go directives

**File:** `compress/code.go:147-169`  
**Claim:** Default code compression removes comments without changing what the
Go code does; the result is re-parsed and declaration counts are checked.  
**Reality:** Some Go comments are compiler directives. `stripComments` removes
all of them, including `//go:build`, `//go:embed`, `//go:generate`,
`//go:linkname`, and cgo preambles. Re-parsing and declaration counting still
succeed, so the safety check accepts semantically different output.

**Reproduction:**

```bash
cd /tmp/tokipe-qa2
go test -count=1 -run TestCodeCompressorPreservesSemanticGoDirectives -v
```

Input contains a Linux build constraint and an embedded-files directive.
Observed output:

```go
package assets

import "embed"

var files embed.FS
```

Both directives were silently removed.

**Impact:** A model receives code with materially different build selection,
embedded assets, cgo declarations, or compiler behaviour while being told it
is a safe compressed representation.

---

### [Major] Default lexical dedupe drops chunks with a critical one-token difference

**File:** `compress/dedupe.go:113-119`, `142-155`  
**Claim:** Defaults are conservative because discarding needed evidence is
worse than sending a duplicate.  
**Reality:** Long templated chunks can exceed the 0.8 shingled-Jaccard
threshold while differing in one decisive value. Two otherwise identical
production policies ending in “30 seconds” and “60 seconds” are treated as
duplicates; the second source is discarded.

**Reproduction:**

```bash
cd /tmp/tokipe-qa2
go test -count=1 -run TestDedupeKeepsCriticalOneTokenDifference -v
```

Observed: only the 30-second policy remains.

**Impact:** RAG can silently remove contradictory or updated evidence and make
the model answer from whichever near-copy happened to rank first.

---

### [Major] The OpenTelemetry module contradicts the documented Go 1.23 minimum

**File:** `metrics/otel/go.mod:3`, `.github/workflows/ci.yml:60-68`,
`README.md:30`  
**Claim:** The project requires Go 1.23+, and the CI matrix tests every nested
module with Go 1.23.  
**Reality:** `metrics/otel` declares `go 1.25.0`. It passed locally only because
`GOTOOLCHAIN=auto` downloaded/selected Go 1.25. With automatic toolchain
switching disabled, Go 1.23 cannot build the module.

**Reproduction:**

```bash
cd metrics/otel
GOTOOLCHAIN=local go build ./...
```

Observed:

```text
go: go.mod requires go >= 1.25.0 (running go 1.23.3; GOTOOLCHAIN=local)
```

**Impact:** A supported Go 1.23 environment cannot use or verify the new OTel
adapter, and CI may silently test it with a downloaded toolchain different from
the configured version.

## 4. Verification results

### Baseline and nested modules

The following passed on revision `093fd6c`:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=0 go build ./...

(cd stores/pgvector && go test -race -count=1 ./...)
(cd toolcache/redis && go test -race -count=1 ./...)
(cd metrics/otel && go test -race -count=1 ./...)
```

The root contains 29 packages. README inventory markers also reproduce exactly:
377 Test/Example functions and 29 root-module packages.

### Benchmark

```bash
go run ./benchmarks
```

Observed:

```text
v1 workload reduction       57.1%
history contribution there  +0.0 percentage points

100-turn long loop
no budget                    195691 billed tokens; peak 4039
history budget                88930 billed tokens; peak 1192 / limit 1200
reduction                     54.6%; 185 messages dropped
```

The two benchmark arms still operate on the same deterministic workload. The
4-chars/token assumption remains clearly disclosed, so no new benchmark defect
was demonstrated.

### Examples

All six examples completed without credentials:

```text
rag-chatbot
local-routing
coding-agent
cli-provider (dry run)
streaming (mock)
observability
```

`go run ./examples/observability -break` preserved 40/40 turns and recorded the
documented 80 degradation events. Finding 3.4 shows that this demonstration
does not cover ordinary cache errors.

### Coverage

Every Delivery §2 coverage number reproduced exactly:

| Package | Coverage | Package | Coverage |
|---|---:|---|---:|
| `agentkit` | 100.0% | `router` | 100.0% |
| `internal/safe` | 100.0% | `preprocess` | 98.9% |
| `rag` | 97.9% | `cache` | 97.2% |
| `stores/mock` | 96.7% | `compress` | 96.4% |
| `budget` | 96.0% | `history` | 94.7% |
| `providers/anthropic` | 93.2% | `toolcache` | 93.0% |
| `metrics` | 91.1% | `providers/cli` | 90.5% |
| `pipeline` | 90.4% | `config` | 89.7% |
| `providers/openai` | 88.9% | `lazyload` | 88.1% |

### Areas reviewed without a demonstrated defect

- Anthropic and OpenAI streaming return pre-stream errors directly, yield
  mid-stream errors, preserve partial text, and close HTTP bodies when the
  consumer abandons the sequence.
- `Run` and `RunStream` share request preparation, stage ordering,
  short-circuit handling, and routing.
- OpenAI wire ordering is system → history → retrieved context → newest turn,
  with the current turn emitted once for both Query representations.
- OpenAI cache breakpoints are not serialized, an empty API key sends no
  Authorization header, and keys do not enter client names.
- Static-prefix and newest-message retention tests passed; no input was found
  that mutates static bytes or writes through the caller's message backing
  array.
- Metric names remained `agentkit.stage_latency_ms` and
  `agentkit.stage_degraded` after the module rename.
- No stale `agentkit/...` import path remains in buildable source.
- All four module paths and local `replace` directives agree.
- `v1.0.0-agentkit-path` points to the old `module agentkit` release and
  `v1.0.0` points to the renamed module, as Delivery §4 describes.
- OTel degradation attributes use only stage and reason; the error string does
  not become a metric label.
- The extension-matrix exemptions that are present are backed by the named
  tests. The matrix's missing new interfaces are covered by Blocker 2.

The real-credential/live-service checks listed in Delivery §3 were not
re-reported as defects. Live CLI streaming was not re-run because it consumes
subscription quota; mock/protocol coverage and the non-live example passed.

## 5. Release decision

**Current decision: NO-GO for Delivery 2**

At minimum, GO requires:

1. update the dependency CI guard for the renamed module and run the exact job;
2. contain every `TokenCounter` call and add it, plus `Summarizer`, to the
   extension completeness guard;
3. make request-cost accounting match the provider-visible current-turn
   representation and allow chunk trimming without a droppable history message;
4. cancel an abandoned CLI subprocess before waiting for it;
5. report ordinary cache backend errors through the degradation sink and audit
   the other silent fail-open sites;
6. preserve semantic Go directives/cgo preambles or refuse those files;
7. prevent lexical dedupe from discarding a near-copy with a meaningful
   difference, or require an explicitly less-conservative opt-in;
8. restore Go 1.23 compatibility for `metrics/otel` or update the documented
   minimum and CI toolchain deliberately;
9. re-run root and nested race suites, examples, benchmarks, evidence commands,
   and every external reproduction above.

The known gaps in Delivery §3 remain release-owner risks and are not part of
this NO-GO.
