# QA & Verification Brief — agentkit Delivery 1

**Audience:** independent reviewers, human or agent, verifying
[DELIVERY-1.md](DELIVERY-1.md) before a v1.0.0 tag.

You are checking work you did not write. Assume nothing in this document is
true until you have run something that shows it. Where a claim here is wrong,
that is itself a finding worth reporting.

---

## 0. Ground rules

**Report what you can demonstrate.** A finding should name a file and line, and
either a failing command, a concrete input that produces wrong output, or a
spec clause the code contradicts. "This looks fragile" is not a finding; "with
`args` containing a NaN float, `HashToolCall` returns X and the spec requires
Y" is.

**Do not re-report the known gaps in [DELIVERY-1.md](DELIVERY-1.md) §3.** They
are deliberate and documented. *Do* report if you find one of them to be worse
than described, or if a fifth one exists that is not listed.

**Do not fix what you find.** Report it. A reviewer who edits the code being
reviewed destroys the independence that makes the review worth anything.

**Severity, and be honest about it:**

| Severity | Meaning |
|---|---|
| **Blocker** | Data loss, a security hole, a broken fail-open guarantee, or a false claim in DELIVERY-1 §2 |
| **Major** | Wrong behaviour under a plausible input; a spec contract violated |
| **Minor** | Correct but misleading, undocumented, or awkward to use |
| **Note** | Observation, question, or improvement idea for a later phase |

Over-reporting severity is not caution — it costs the next reader real time.

## 1. Getting oriented

Read in this order. Do not skip to the code.

1. [spec.md](spec.md) §1 (why) and §2 (the contract) — the source of truth
2. [DELIVERY-1.md](DELIVERY-1.md) — what is claimed, and what is not
3. [../PLAN.md](../PLAN.md) — decisions and documented deviations
4. [../README.md](../README.md) — the intended shape from a caller's view
5. `pipeline/stage.go` — every other package is downstream of this file

Baseline commands, expected to be green before you start. If any fails, stop
and report that first — everything downstream is suspect:

```bash
go build ./... && go vet ./... && go test -race ./...
CGO_ENABLED=0 go build ./...
go run ./benchmarks
```

## 2. Verification areas

Each area below names what to check and the property that must hold. Pick up
whichever area you are assigned; they are independent.

### A. The fail-open guarantee (spec §2.5.1) — highest value

This is the load-bearing promise: **no optimization may cause `Pipeline.Run` to
fail.** The only errors that may escape are from the final `ModelClient.Send`,
or a nil required dependency.

- For every stage, force its dependency to fail — cache backend down, embedder
  erroring, compressor returning an error, tool executor panicking, parser
  returning garbage — and confirm the turn still reaches the model.
- Look for any `return nil, err` in a stage's `Process`. Each one is either a
  bug or needs a comment justifying it.
- Check the seams too: `config.Stages()` with half-configured options, a nil
  `metrics.Recorder`, an empty tier list.

### B. Cache-breakpoint placement (spec §2.4.6) — highest risk

The non-negotiable rule: **never anchor a breakpoint after content that changes
every turn.** Getting this wrong is worse than not caching, because it pays the
cache-write premium on every call and never reads.

- `cache/breakpoint.go` — is `computeBreakpoints` genuinely pure and
  deterministic? Try to find an input where two identical requests produce
  different breakpoints.
- Try to construct a `Request` where a breakpoint lands at or after the
  retrieved-chunk segment. The tests assert it cannot; try to prove them wrong.
- `config.Stages()` claims the ordering is un-misconfigurable. Try to
  misconfigure it — particularly via `WithStage`.

### C. Determinism (spec §2.5.4)

- `toolcache.HashToolCall` — same args, different construction order, nested
  maps, slices of maps, numeric types (`int` vs `float64` vs `json.Number`),
  `nil` values, non-UTF-8 strings. Any pair that *should* hash the same but
  does not is a correctness bug; any pair that should differ but collides is
  worse.
- `cli.RenderPrompt` — must be byte-identical across runs for identical input.
  Map iteration order is the classic leak.

### D. Concurrency

- `go test -race ./...` is necessary, not sufficient. The race detector only
  sees what executes.
- `toolcache/singleflight.go` — a hand-rolled singleflight, written because the
  core is stdlib-only. Read it adversarially: panics, a leader that never
  returns, a follower arriving as the leader deletes the key, `inFlight`
  leaking entries.
- Shared-state components: `MemoryCache`, `Aligner`, `Stage`, `HeuristicRouter`.
  Any of them holding per-request state across goroutines is a Blocker.

### E. Security

- `providers/cli` executes subprocesses. Verify no path reaches a shell.
  Check argv construction, `PromptPlaceholder` substitution, and `Config.Env`.
- `lazyload.FileLoader` — path traversal. Try `..`, absolute paths, symlinks
  out of the root, `.` tricks, and platform-specific separators.
- `stores/pgvector` — identifier validation is the only defence against
  injection, since identifiers cannot be parameterised. Try to defeat it.
- Anywhere an API key could reach a log, an error string, `Name()`, or a
  process argument.

### F. Claims in DELIVERY-1 §2

Re-run each one. Do not take the table's word for it. The benchmark in
particular: read `benchmarks/main.go`, decide whether the baseline arm is a
fair comparison or a strawman, and say so plainly. A rigged baseline would make
the headline number worthless, so this is worth real scrutiny.

### G. Spec conformance

Walk spec §2.3–§2.4 signature by signature against the code. Deviations are
listed in DELIVERY-1 §4 — an *undocumented* deviation is a finding.

## 3. Environment notes

- Go 1.23+. `-race` needs cgo; the `CGO_ENABLED=0` check is build-only.
- Nested modules are separate and not covered by a root-level `go test ./...`:

  ```bash
  cd stores/pgvector && go test ./...
  cd toolcache/redis  && go test ./...
  ```

- Tests requiring credentials or external services, all skipping by default:

  | Test | Gate | Cost |
  |---|---|---|
  | `TestPromptCachingAcrossTurns` | `ANTHROPIC_API_KEY` | two billed API calls |
  | `TestLiveCLIs` | `AGENTKIT_CLI_LIVE=1` | subscription quota, ~15s |
  | pgvector integration | `AGENTKIT_PGVECTOR_DSN` | needs a database |

- `examples/cli-provider` defaults to a dry run and exits 0 with no CLI
  installed. `-live` spends real quota.

## 4. Reporting format

One finding per entry. Group by severity, highest first.

```
[Severity] Short title
  File:    path/to/file.go:123
  Claim:   what the code or docs assert
  Reality: what actually happens
  Repro:   the exact command or test that shows it
  Impact:  who is hurt and how
```

Close with a plain **go / no-go** for tagging v1.0.0, and say what would change
it. If your area was clean, say that explicitly — a silent area is
indistinguishable from an unexamined one.

## 5. Out of scope for this delivery

Not defects; do not report them:

- Missing features the spec puts in a later phase (AST-aware compression, a
  learned router or compressor, dashboards, hosted service, multi-tenant
  billing) — spec §1.3, §2.4.4
- The module path still being `agentkit` — it changes at tag time
- Absence of a git remote
- Style preferences with no behavioural consequence
