# QA Verification Report — tokipe Delivery 2

**Review date:** 2026-07-27  
**Revisions:** Round 1 `093fd6c`; Round 2 `9176327`

**Scope:** [DELIVERY-2.md](DELIVERY-2.md), ROADMAP Phases 5–8 and the module-path rename  
**Procedure:** [QA-BRIEF.md](QA-BRIEF.md)  
**Baseline:** [DELIVERY-1.md](DELIVERY-1.md)  
**Current result:** **GO after QA Round 4**

## 1. Executive summary

The Round 2 revision resolves both release blockers and four of the seven Major
findings from Round 1. The exact CI dependency command now passes,
caller-supplied token counters are contained throughout trimming, history
budgeting matches the provider-visible request, chunk-only requests can be
trimmed, ordinary cache failures are observable, and the OpenTelemetry module
builds locally with Go 1.23.

At the Round 2 QA checkpoint, three Major findings remained open. Abandoning a
shell-backed CLI stream could still wait for a descendant that inherited
stdout; the code-compression guard missed valid cgo preambles without
`#include` or `#cgo`; and default deduplication could discard a long near-copy
whose decisive difference was nonnumeric.

Round 2 also introduced one release blocker: the coverage table in
Delivery §2 was carried forward unchanged even though the implementation and
tests changed. Six listed values no longer reproduce, so the evidence table is
false under the QA brief's explicit Blocker rule.

QA Round 3 independently reproduced the refreshed evidence and verified the
coverage, CLI process-tree, and cgo remediations. At that checkpoint, the
dedupe remediation remained incomplete: a threshold of 1.0 over a *set* of
shingles is not an exact normalized-sequence comparison. Distinct periodic
sequences can have identical shingle sets and the second chunk is still
discarded.

The implementor follow-up in §5 changes the threshold-1 path to compare the
complete normalized word sequence directly. Its regression and full local
verification pass. QA Round 4 exercised periodic rotations, normalized
formatting equivalence, the complete external repro suite, and the release
evidence; no current finding remains.

The documented baseline remains healthy. The root module builds and vets,
passes its race suite across 29 packages, builds without CGo, and reproduces
both benchmark results: 57.1% on the v1 workload and 54.6% over the 100-turn
history workload. All three nested modules pass their race suites with
`GOTOOLCHAIN=local`, all six examples complete, and the broken-dependencies
observability example keeps 40/40 turns alive.

| Round 2 QA severity | Count |
|---|---:|
| Blocker | 1 |
| Major | 3 |
| Minor | 0 |
| Note | 0 |

| Current Round 3 severity | Count |
|---|---:|
| Blocker | 0 |
| Major | 1 |
| Minor | 0 |
| Note | 0 |

| Current Round 4 severity | Count |
|---|---:|
| Blocker | 0 |
| Major | 0 |
| Minor | 0 |
| Note | 0 |

The external reproductions were placed in `/tmp/tokipe-qa2`; no production
source code was changed during this review.

## 2. Round 2 re-verification

| Round 1 finding | Round 2 result |
|---|---|
| B1 — renamed-module CI guard | **Resolved** — the exact CI command returns no third-party dependencies |
| B2 — `TokenCounter` panic escapes | **Resolved** — the external panic reproduction reaches the model |
| M1 — duplicated current turn is over-counted | **Resolved** |
| M2 — chunk-only over-budget request is not trimmed | **Resolved** |
| M3 — abandoned CLI stream waits for the child | **Open** — see 2.2 |
| M4 — ordinary cache `Get` error is invisible | **Resolved** |
| M5 — semantic Go comments are removed | **Open** — directives pass, but valid cgo remains unsafe; see 2.3 |
| M6 — decisive one-token difference is deduplicated | **Open** — numeric values pass, but nonnumeric decisions remain unsafe; see 2.4 |
| M7 — OTel contradicts Go 1.23 minimum | **Resolved** — nested race suite passes with `GOTOOLCHAIN=local` |

### 2.1 [Blocker] The current coverage evidence no longer reproduces

**File:** `docs/DELIVERY-2.md`, §2 coverage table

**Claim:** The listed percentages are the coverage after the Round 2 delivery.

**Reality:** Adding implementation branches and regression tests changed six
listed package values, but the table was not refreshed.

**Reproduction:**

```bash
go test -count=1 -cover ./...
```

| Package | Delivery claim | Round 2 observed |
|---|---:|---:|
| `budget` | 96.0% | 96.1% |
| `compress` | 96.4% | 94.1% |
| `history` | 94.7% | 94.4% |
| `preprocess` | 98.9% | 96.7% |
| `providers/cli` | 90.5% | 90.3% |
| `toolcache` | 93.0% | 93.2% |

**Impact:** Delivery §2 contains evidence that cannot be reproduced at the
revision it describes. The QA brief explicitly classifies a false evidence
claim as a Blocker. This does not mean the test suite fails; it means the
handover evidence is stale and several newly added branches reduced measured
coverage.

### 2.2 [Major] Abandoning a shell-backed CLI stream still waits for a descendant

**File:** `providers/cli/stream.go:91-116`

**Claim:** Cancelling before reaping makes an abandoned stream return
immediately.

**Reality:** `exec.CommandContext` kills the direct process, but a descendant
such as `sleep` can retain the inherited stdout pipe. `reap` drains that pipe
before `Wait`, so it remains blocked until the descendant exits. The new
in-repository test uses the test binary directly and does not exercise a
process tree.

**Reproduction:**

```bash
cd /tmp/tokipe-qa2
go test -count=1 -run TestAbandoningCLIStreamReturnsPromptly -v
```

The command is `sh -c "printf ...; sleep 2; printf ..."`. Observed: breaking
after the first delta still takes approximately 2.01 seconds rather than less
than 500 ms.

**Impact:** Shell scripts and wrapper CLIs can still retain a request goroutine
and delay disconnect cleanup until their descendants exit or the timeout is
reached.

### 2.3 [Major] The cgo guard recognizes markers, not the cgo preamble relationship

**File:** `compress/code.go:159-217`

**Claim:** Directive-bearing files and cgo preambles are refused outright.

**Reality:** A block comment is classified as cgo only when its text contains
`#include` or `#cgo`. A valid cgo preamble may instead contain ordinary C
declarations or definitions immediately before `import "C"`. Such a file is
still claimed by `CanHandle`, and compression removes the C source while
producing syntactically valid Go.

**Reproduction:**

```bash
cd /tmp/tokipe-qa2
go test -count=1 -run TestCodeCompressorRefusesArbitraryCgoPreamble -v
```

Observed: `CanHandle` returns true for a preamble containing
`int answer(void) { return 42; }`.

**Impact:** Compression can silently change or break a valid cgo file. The
guard must identify the comment group attached to `import "C"`, not infer cgo
from two possible strings inside the comment.

### 2.4 [Major] Nonnumeric decisive differences still pass the dedupe safety guard

**File:** `compress/dedupe.go:150-168`, `221-250`

**Claim:** Default dedupe does not discard a near-copy with a meaningful
difference.

**Reality:** The new fact-agreement guard extracts only words containing a
digit. Two sufficiently long documents with identical numeric facts but an
opposite final decision such as `allow` versus `deny` still exceed the 0.95
Jaccard threshold and the second chunk is discarded.

**Reproduction:**

```bash
cd /tmp/tokipe-qa2
go test -count=1 -run TestDedupeKeepsCriticalNonnumericDifference -v
```

Observed: only the `allow` policy remains; the otherwise identical `deny`
policy is removed.

**Impact:** Retrieval can silently lose contradictory evidence and make the
answer depend on chunk ordering. A conservative default needs an exact-content
rule, a stronger difference guard, or explicit opt-in to lossy near-deduping.

## 3. Implementor follow-up after Round 2

The working tree after base revision `9176327` contains these remediations:

| Round 2 finding | Implemented remediation | Implementor verification |
|---|---|---|
| Stale coverage evidence | Recomputed after the final implementation and updated Delivery §2 and README inventory | 393 Test/Example functions; final table reproduced |
| Shell-descendant stream wait | Unix process-group termination, explicit stdout close before reap, and a 100 ms `exec.Cmd.WaitDelay` for inherited stdout/stderr pipes | Process-tree regression passed 10 consecutive runs; original external repro passed |
| Unrecognized cgo preamble | Refuse every parsed file containing `import "C"` | Arbitrary-C-preamble regression and original directive repro passed |
| Nonnumeric dedupe conflict | Default threshold changed to 1.0; lower lossy thresholds remain explicit opt-in | `allow`/`deny`, numeric-conflict, near-formatting, and exact-copy tests passed |

The following also passed after implementation:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 go test -c ./providers/cli

(cd stores/pgvector && GOTOOLCHAIN=local go test -race -count=1 ./...)
(cd toolcache/redis && GOTOOLCHAIN=local go test -race -count=1 ./...)
(cd metrics/otel && GOTOOLCHAIN=local go test -race -count=1 ./...)

go run ./benchmarks
go run ./examples/<all-six>
go run ./examples/observability -break
(cd /tmp/tokipe-qa2 && go test -count=1 ./...)
```

Final implementor-observed coverage values:

| Package | Coverage | Package | Coverage |
|---|---:|---|---:|
| `agentkit` | 100.0% | `router` | 100.0% |
| `internal/safe` | 100.0% | `preprocess` | 96.7% |
| `rag` | 97.9% | `cache` | 97.2% |
| `stores/mock` | 96.7% | `budget` | 96.1% |
| `compress` | 94.2% | `history` | 94.4% |
| `providers/anthropic` | 93.2% | `toolcache` | 93.2% |
| `metrics` | 91.1% | `providers/cli` | 90.0% |
| `pipeline` | 90.4% | `config` | 89.7% |
| `providers/openai` | 88.9% | `lazyload` | 88.1% |

## 4. QA Round 3 re-verification

Three of the four Round 2 follow-up findings are resolved:

| Finding | QA Round 3 result |
|---|---|
| Coverage evidence | **Resolved** — all Delivery §2 percentages, 393 test functions, and 29 root packages reproduce |
| Shell-descendant stream wait | **Resolved** — the original shell repro passes and the in-repository process-tree test passed 20 consecutive runs |
| Arbitrary cgo preamble | **Resolved** — both compiler-directive and ordinary-C-preamble repros pass |
| Dedupe exact-match default | **Open** — distinct normalized sequences can share the same shingle set |

### [Major] Default dedupe still drops distinct normalized sequences

**File:** `compress/dedupe.go:56-63`, `192-202`

**Claim:** `DELIVERY-2.md` §0.1 and the implementation comment describe the
default threshold of 1.0 as a normalized exact match, with lossy matching
requiring explicit opt-in.

**Reality:** Jaccard compares sets of four-word shingles. A set discards
multiplicity and global sequence position. Two different periodic sequences
can therefore produce identical shingle sets, score 1.0, pass the fact-token
guard, and be treated as duplicates.

**Reproduction:**

```bash
cd /tmp/tokipe-qa2
go test -count=1 -run TestDefaultDedupeRequiresExactNormalizedSequence -v
```

Observed: the two distinct policies below collapse to one chunk:

```text
allow users deny admins allow users deny admins ...
deny admins allow users deny admins allow users ...
```

**Impact:** Default configuration can still discard distinct retrieved
evidence despite documenting that lossy near-deduplication is opt-in. This is
wrong behavior on plausible repetitive policy, log, or generated content.

The rest of the release-candidate verification passed:

```text
root build, vet, race suite, and CGO-disabled build
three nested race suites with GOTOOLCHAIN=local
Windows providers/cli compile check
all nine earlier external reproductions
dependency guard, 393-function inventory, and 29-package inventory
all Delivery coverage percentages
57.1% and 54.6% benchmark results
all six examples; observability fail-open 40/40 with 80 degradations
```

## 5. Implementor follow-up after QA Round 3

The Round 3 reproduction is now an in-repository regression test. For threshold
1.0, `DedupeStage` uses exact slice equality over `normalizeWords` output.
Thresholds below 1 retain Jaccard and the value-token guard as explicit lossy
behavior.

Implementor verification passed:

```text
Round 3 exact-sequence regression
all earlier external QA reproductions
dedupe exact-copy, formatting-only, numeric-conflict, and custom-threshold tests
root and all three nested race suites
CGO-disabled build and Windows providers/cli compile
dependency guard, 394-function inventory, and 29 root packages
all examples and observability fail-open
benchmarks: 57.1% and 54.6%
```

Final observed `compress` coverage is 93.3%; the other Delivery values are
unchanged.

## 6. QA Round 4 re-verification

**Result: GO for the current working tree**

The Round 3 defect is resolved. Default dedupe now compares the complete
normalized word sequence, while lower thresholds retain explicitly lossy
Jaccard behavior.

Adversarial verification passed:

- the original Round 3 shingle-set collision;
- every periodic rotation for periods 2 through 8;
- 100 consecutive runs of the in-repository exact-sequence regression;
- formatting and case variants with the same normalized sequence still
  deduplicate;
- numeric and nonnumeric policy differences remain separate;
- exact copies and explicitly configured lower thresholds retain their intended
  behavior;
- all earlier external QA reproductions.

Release evidence also passed:

```text
root build, vet, race suite, and CGO-disabled build
three nested race suites with GOTOOLCHAIN=local
Windows providers/cli compile check
dependency guard: no third-party core dependencies
394 Test/Example functions and 29 root packages
all Delivery coverage values, including compress 93.3%
benchmarks: 57.1% and 54.6%
all six examples
observability fail-open: 40/40 turns and 80 degradations
```

No production code was changed during QA Round 4. Additional property checks
remain outside the repository under `/tmp/tokipe-qa2`.

## 7. Round 1 blockers (resolved in Round 2)

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

## 8. Round 1 Major findings

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

## 9. Round 2 QA verification results

### Baseline and nested modules

The following passed again on Round 2 revision `9176327`:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=0 go build ./...

(cd stores/pgvector && GOTOOLCHAIN=local go test -race -count=1 ./...)
(cd toolcache/redis && GOTOOLCHAIN=local go test -race -count=1 ./...)
(cd metrics/otel && GOTOOLCHAIN=local go test -race -count=1 ./...)
```

The root contains 29 packages. README inventory markers also reproduce exactly:
390 Test/Example functions and 29 root-module packages.

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
documented 80 degradation events. The direct ordinary-cache-error regression
also passed.

### Coverage

The current Round 2 values are below. Six differ from Delivery §2, as recorded
in Blocker 2.1:

| Package | Coverage | Package | Coverage |
|---|---:|---|---:|
| `agentkit` | 100.0% | `router` | 100.0% |
| `internal/safe` | 100.0% | `preprocess` | 96.7% |
| `rag` | 97.9% | `cache` | 97.2% |
| `stores/mock` | 96.7% | `compress` | 94.1% |
| `budget` | 96.1% | `history` | 94.4% |
| `providers/anthropic` | 93.2% | `toolcache` | 93.2% |
| `metrics` | 91.1% | `providers/cli` | 90.3% |
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
- The extension matrix now includes `budget.TokenCounter` and
  `history.Summarizer`; both panic-contract cases passed.

The real-credential/live-service checks listed in Delivery §3 were not
re-reported as defects. Live CLI streaming was not re-run because it consumes
subscription quota; mock/protocol coverage and the non-live example passed.

## 10. Release decision

**Current decision: GO for Delivery 2**

This GO applies to the exact current working tree. Commit and tag that state
without additional code changes; otherwise rerun the proportional checks.

The known live-service and real-world integration gaps in Delivery §3 remain
release-owner risks and are not represented as verified by this GO.
