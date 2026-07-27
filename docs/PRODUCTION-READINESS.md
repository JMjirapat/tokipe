# Production Readiness Review — tokipe

**Date:** 2026-07-27
**Revision reviewed:** `9176327` plus the uncommitted QA round 2–4 working tree
**Verdict at review time:** **Code is ready. The release is not.**
**Verdict now:** all five findings resolved; published as `v1.0.0` and verified
against the public proxy (§7), first CI run green including the pgvector
integration job (§8), prompt caching confirmed against a live endpoint (§9).

Every command in this document was executed, not quoted from an earlier
document. Where a claim could only be settled by reproducing a failure, it was
reproduced.

---

## 0. Status

| # | Finding | Status |
|---|---|---|
| B1 | QA-approved work uncommitted | **Fixed** — committed |
| B2 | `v1.0.0` predates every QA fix | **Fixed** — `v1.0.0` moved to the final commit; `stores/pgvector`, `toolcache/redis` and `metrics/otel` tagged too |
| B3 | Two nested modules require an unresolvable version | **Fixed and confirmed** — both require `v1.0.0`; verified against the real proxy after publication (§7) |
| D1 | `agentkit` vs `tokipe` naming | **Resolved** — renamed to `tokipe` throughout |
| D2 | Benchmark label carries the full module path | **Fixed** |

B3's failure mode only reproduces against a published proxy, so it could not be
settled locally. It has since been confirmed there — see §7.

---

## 1. Summary

The library itself passes everything asked of it. At review time, five
release-mechanics defects stood between the tree and a publishable v1.0.0, and
three of them would have broken real users on the first `go get`. All five have
since been resolved — §3 and §4 describe them as found, §0 records where each
one landed.

| Area | Status |
|---|---|
| Code correctness | Ready — QA Round 4 GO, independently re-verified |
| Test and coverage | Ready — 518 tests, 29 packages, `-race` clean |
| Portability | Ready — Go 1.23, `CGO_ENABLED=0`, linux/windows/darwin |
| Dependency guarantee | Ready — core module has zero third-party deps |
| Measured benefit | Ready — 57.1% and 54.6% token reduction |
| **Release mechanics** | **Was not ready — 3 blocking, 2 decisions; all resolved and published** |

---

## 2. Verification performed

Reproduce from a clean checkout of the reviewed tree:

```bash
go build ./... && go vet ./... && go test -race -count=1 ./...
CGO_ENABLED=0 go build ./...
GOOS=windows go build ./... && GOOS=linux go build ./...
go run ./benchmarks
```

| Check | Command | Result |
|---|---|---|
| Build and vet | `go build ./... && go vet ./...` | clean |
| Full race suite | `go test -race -count=1 ./...` | **518 pass, 29 packages, 394 test functions** |
| No CGo | `CGO_ENABLED=0 go build ./...` | clean |
| Cross-compile | `GOOS=windows`, `GOOS=linux` | both build |
| Zero third-party deps | the exact CI command, re-run verbatim | **none found** |
| Nested modules on Go 1.23 | `GOTOOLCHAIN=local go test -race ./...` | pgvector 14, redis 7, otel 7 |
| Token reduction | `go run ./benchmarks` | **57.1%** over 12 turns (target ≥30%) |
| Long-loop budget | same, long-loop section | **54.6%** over 100 turns, peak 1192 vs a 1200 limit |
| Examples | `go run ./examples/<name>` | all six complete, no credentials |
| README inventory guard | `go test ./...` (inventory_test.go) | markers match the tree |

`GOTOOLCHAIN=local` matters here: it proves the nested modules actually build
on Go 1.23 rather than silently pulling a newer toolchain, which is exactly how
finding M7 escaped the first time.

### 2.1 Coverage

| Package | Cov | | Package | Cov |
|---|---|---|---|---|
| root | 100.0% | | `providers/anthropic` | 93.2% |
| `router` | 100.0% | | `toolcache` | 93.2% |
| `internal/safe` | 100.0% | | `compress` | 93.3% |
| `rag` | 97.9% | | `pipeline` | 90.4% |
| `cache` | 97.2% | | `providers/cli` | 90.0% |
| `preprocess` | 96.7% | | `config` | 89.7% |
| `budget` | 96.1% | | `providers/openai` | 88.9% |
| `history` | 94.4% | | `lazyload` | 88.1% |

`internal/safe` at 100% is the one that matters most: it is the containment
boundary the entire fail-open guarantee rests on.

---

## 3. Blocking findings

### B1 — The QA-approved work is uncommitted

Thirteen modified files plus two new ones (`providers/cli/process_unix.go`,
`providers/cli/process_fallback.go`) exist only in the working tree.

**The Round 4 GO verdict was issued against the working tree, not against any
commit.** No commit in this repository currently contains the code that passed
QA. A lost working directory loses QA rounds 2 through 4.

**Fix:** commit the tree before anything else happens.

### B2 — `v1.0.0` points at code with every QA defect still in it

```
d767f48  Phase 8
0225dc1  rename to github.com/JMjirapat/tokipe
6654cdc  docs                        ← v1.0.0 points here
093fd6c  docs: DELIVERY-2
9176327  fix: nine QA findings       ← QA round 1 lands after the tag
(working tree)                       ← QA rounds 2–4 land here
```

The tag predates every fix. Anyone resolving `@v1.0.0` receives the nine Round 1
defects plus the four from Round 2 — including a `TokenCounter` panic that
escapes `Pipeline.Run`, a dedupe that discards `allow` in favour of `deny`, and
a `CodeCompressor` that eats cgo preambles.

The tag has also already been **moved once**: the original release commit is
`d6f6429`, preserved as `v1.0.0-agentkit-path`. That is harmless only because
nothing has been pushed. **Once a tag reaches the Go module proxy it is cached
permanently and cannot be corrected** — a moved tag after publication produces a
checksum mismatch for every consumer who fetched the earlier one.

**Fix:** delete the local `v1.0.0`, tag the final commit, and never move it again
after pushing.

### B3 — Two nested modules cannot be consumed by anyone

| Module | Requires |
|---|---|
| `stores/pgvector` | `github.com/JMjirapat/tokipe v0.0.0` |
| `toolcache/redis` | `github.com/JMjirapat/tokipe v0.0.0-00010101000000-000000000000` |
| `metrics/otel` | `github.com/JMjirapat/tokipe v1.0.0` ✅ |

A `replace` directive is **ignored when its module is consumed as a
dependency** — it applies only to the main module. Downstream, the `require`
line must resolve on its own. `v0.0.0` has no corresponding tag, and
`v0.0.0-00010101000000-000000000000` is the placeholder Go writes for a replaced
module; it is unresolvable by construction.

Reproduced with a probe module standing in for a downstream consumer:

```
go: downloading github.com/JMjirapat/tokipe v0.0.0
reading github.com/JMjirapat/tokipe/go.mod at revision v0.0.0: ... not found
```

Separately, all three nested modules need their **own** tags
(`stores/pgvector/v1.0.0`, `toolcache/redis/v1.0.0`, `metrics/otel/v1.0.0`)
before `go get` can install them at all.

**Fix:** pin each nested module's `require` to a version that will actually
exist, and tag the submodules.

---

## 4. Decisions taken before tagging

### D1 — Module, package and metric names disagreed — **resolved: renamed**

As reviewed, the identity was split three ways:

| | As reviewed | Now |
|---|---|---|
| Module | `github.com/JMjirapat/tokipe` | unchanged |
| Package | `agentkit` — callers wrote `agentkit.New()` | `tokipe` |
| Metrics | `agentkit.stage_latency_ms` | `tokipe.stage_latency_ms` |
| Env | `AGENTKIT_CLI_LIVE`, `AGENTKIT_PGVECTOR_DSN` | `TOKIPE_*` |
| README | `# agentkit` | `# tokipe` |

The split was legal Go and it worked. It was also confusing: a user ran
`go get github.com/JMjirapat/tokipe` and then typed `agentkit.New`.

The reason it belonged in a release review rather than a backlog is that **v1.0
would have frozen it**. A package name is part of every importer's source; the
metric names are part of every dashboard and alert rule built on the library.
Changing either after publication breaks both. Before the tag it costs a
mechanical rename; after it, a major version.

Renaming was chosen and applied across all Go sources and every forward-looking
document. Two categories were deliberately left alone:

- **`v1.0.0-agentkit-path`** is a real tag. A first pass renamed it in prose,
  which would have pointed readers at a tag that does not exist.
- **`docs/spec.md`, `docs/DELIVERY-1.md`, `docs/QA-REPORT.md`,
  `docs/QA-REPORT-2.md`** are dated records of work reviewed under the old name.
  Rewriting them would claim a review happened against an artifact that did not
  yet exist.

### D2 — Collateral from the module rename — **resolved**

`benchmarks/main.go:73` passed the full module path where a short arm label
belongs, so the benchmark printed:

```
github.com/JMjirapat/tokipe  billed=  2148
```

That was the **only** string literal the earlier module-path rename damaged —
every metric name survived it intact, which was checked explicitly across the
whole tree before concluding so.

---

## 5. Recommended sequence

1. ~~Commit the QA round 2–4 work.~~ **done**
2. ~~Decide `agentkit` versus `tokipe`.~~ **done — renamed**
3. ~~Fix the nested modules' `require` versions.~~ **done**
4. ~~Fix the benchmark label.~~ **done**
5. ~~Delete the local `v1.0.0`; re-tag the final commit; tag the three submodules.~~ **done**
6. ~~Push, then verify `go get` end to end from a scratch module.~~ **done — see §7**

Step 6 was not optional. Publication is irreversible: the proxy caches every tag
it sees, so a mistake found after the push cannot be withdrawn, only superseded
by a new version. **`v1.0.0` and the three nested-module tags are now published
and must never be moved.**

---

## 7. Post-publication verification

`main` and all five tags are pushed. Every check below ran against
`proxy.golang.org` and `sum.golang.org`, from scratch modules, with the tokipe
entry removed from the local module cache first.

| Check | Result |
|---|---|
| `go get github.com/JMjirapat/tokipe` | resolves `v1.0.0` |
| `go get` each nested module, no version suffix | all three resolve |
| Import + `go mod tidy` + `go build` for each nested module | all three build |
| Consumer program: `tokipe.New` → `kit.Run` | runs, returns the expected content |
| Proxy `.info` / `.mod` / `.zip` for all three nested modules | HTTP 200, correct module paths |

**B3 is confirmed fixed against a real proxy** — the failure it addressed could
only ever be reproduced there.

### A false alarm worth recording

An intermediate run appeared to show `toolcache/redis` and `metrics/otel`
failing while `stores/pgvector` passed:

```
go: module github.com/JMjirapat/tokipe@v1.0.0 found, but does not contain
    package github.com/JMjirapat/tokipe/toolcache/redis
```

That was **an artifact of the test loop, not a defect**. `stores/pgvector` was
simply first in the loop every time. Whichever nested module runs first against
a cold cache succeeds; the rest fail, because by then the *root* module is
cached, and `go get <package-path>@<version>` matches that shorter prefix and
stops instead of querying the deeper module path. Reversing the loop order moved
the failure onto the other two and cleared `stores/pgvector`, which is what
identified the cause.

Two lessons, both cheap to forget:

- A loop that always tests in the same order is not three independent tests.
- The first result in a cache-sensitive sequence is the only unbiased one.

The behaviour is a `go get` quirk, not a packaging fault: the bare `go get`, the
import-and-tidy path, and an explicit `require` plus `go mod download` all
succeed for every nested module. It is documented in the README so users who hit
the message know what to do.

---

## 6. Known limitations

These are documented rather than defects, and none blocks release. The two that
were "never run" have since been run — both are struck through below.

- ~~**Anthropic prompt-caching test has never run against the live endpoint.**~~
  **Closed — see §9.** It has now passed against a live Messages-API endpoint.
  It still skips without a key, so CI stays green.
- ~~**The pgvector CI job has never executed.**~~ **Closed — it has now run
  against a real database, both in CI and locally. See §8.**
- **Chunk dedupe is lexical, not semantic.** It catches copies and near-copies.
  Two passages saying the same thing in different words need embeddings, which
  is a Phase 9 question, not a silent upgrade to this one.
- **The CLI provider cannot measure cache alignment.** CLI backends do not report
  cache token counts; alignment there is a correctness property, not a measured
  one.

---

## 8. First CI run

Two runs on `main`, both green, **every job executed** — none skipped:

| Job | Result | Duration |
|---|---|---|
| core module | success | 63s |
| nested module `stores/pgvector` | success | 51s |
| nested module `toolcache/redis` | success | 45s |
| nested module `metrics/otel` | success | 42s |
| **pgvector integration (real database)** | **success** | **73s** |

The last one had never run anywhere before this push. It is also the one where a
green result is easiest to fake: a credential-gated test that skips looks
identical to one that passes. The workflow already guards against that with a
second step that greps the verbose output and fails on `--- SKIP`:

```yaml
- name: fail if the integration tests were skipped
```

That step reports **success**, so the tests genuinely executed against
`pgvector/pgvector:pg16` rather than skipping past it.

Reproduced independently on the development machine, which does have Docker
after all:

```bash
docker run -d --name tokipe-pgv -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=postgres -p 55432:5432 pgvector/pgvector:pg16
cd stores/pgvector && TOKIPE_PGVECTOR_DSN='postgres://postgres:postgres@localhost:55432/postgres?sslmode=disable' \
  go test -race -count=1 -run TestIntegration -v ./...
```

```
=== RUN   TestIntegrationSearch
--- PASS: TestIntegrationSearch (0.08s)
ok  github.com/JMjirapat/tokipe/stores/pgvector  1.542s
```

Real database, real query, no skip. The pgvector limitation is closed.

The Anthropic prompt-caching test needed API credit rather than Docker; it has
since been run too — see §9.

---

## 9. Prompt caching against a live endpoint

`TestPromptCachingAcrossTurns` **passes**. This was the last outstanding
limitation and it is now closed.

| | |
|---|---|
| Run by | the maintainer, not the author of this document |
| Endpoint | `https://api.maxplus-ai.cc` — a gateway speaking the Messages API |
| Model | `claude-sonnet-5` |
| Result | pass |

What that proves, precisely: the client emits `cache_control` in a form a real
Messages-API server accepts, the static prefix is byte-identical across turns,
and `cache_read_input_tokens` / `cache_creation_input_tokens` are parsed back
correctly. The assertions cannot pass without a real cache write on turn 1 and a
real cache read on turn 2.

What it does not prove: behaviour against `api.anthropic.com` itself. The run
went through a gateway. That is a genuine end-to-end result — a gateway that
stripped `cache_control` or dropped the usage fields would have failed the test,
which is why those two failure modes are named in the error messages — but a run
against Anthropic's own endpoint is still not on record, and the distinction is
worth keeping rather than rounding away.

### Why it took two attempts

The first invocation reported `--- SKIP` with the key apparently set. The cause
was neither the key nor the code: blank lines between the `\` continuations
broke the line continuation, so the shell executed three separate assignments
that were never exported, and `go test` ran with none of them.

```
with blank lines between continuations:  FOO=[] BAR=[]
without:                                 FOO=[dummy] BAR=[two]
```

The test worked exactly as designed — it simply never received the variables.
The `endpoint=… model=…` line it logs first exists for this: if that line is
absent, the environment did not reach the test, and no assertion below it ran.
