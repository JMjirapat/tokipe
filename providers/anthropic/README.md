# providers/anthropic

A `pipeline.ModelClient` for the Anthropic Messages API, built on `net/http`
alone — it adds no third-party dependency, so it ships inside the core module
without breaking the zero-dependency guarantee.

## Usage

```go
client, err := anthropic.New(anthropic.Config{
    APIKey:    os.Getenv("ANTHROPIC_API_KEY"),
    Model:     "claude-opus-5",
    MaxTokens: 1024,
})
if err != nil {
    return err
}

kit := tokipe.New(client, config.WithCacheAlignment())
```

The client implements both `pipeline.ModelClient` and
`pipeline.StreamingClient`. Use `kit.Run` for a completed response or
`kit.RunStream` for incremental SSE text and final usage.

For exact history-budget accounting, reuse the same client through the token
counter:

```go
counter, err := anthropic.NewTokenCounter(client)
if err != nil {
    return err
}

kit := tokipe.New(client,
    config.WithHistoryBudget(budget.DefaultPolicy(), counter),
    config.WithCacheAlignment(),
)
```

The counter calls `/v1/messages/count_tokens` and memoizes repeated string
counts. It adds a network round trip but is more reliable than the default
character estimator for hard context limits.

## What it does with `CacheBreakpoints`

`Request.CacheBreakpoints` become `cache_control: {"type": "ephemeral"}` markers
on the outgoing content blocks. The API accepts at most **four**; when a request
carries more, the client keeps the four deepest (latest) breakpoints and drops
the rest, since the longest prefix is the one worth caching.

`Response.Usage` is populated from the API's `usage` object, including
`cache_read_input_tokens` and `cache_creation_input_tokens` — those two fields
are how you verify caching is actually working in production.

## Manual integration test

> **Status:** passing. Last recorded run went through a Messages-API gateway
> with `claude-sonnet-5`; turn 1 wrote the prefix to the cache and turn 2 read
> it back. A run against `api.anthropic.com` itself is not yet on record.

Unit tests run against `httptest` and need no credentials. The caching
behaviour, however, can only be confirmed against the real endpoint.

**You need Anthropic API credit for this, not a Claude subscription.** A Claude
Pro or Max plan authenticates the Claude apps and Claude Code; it does not issue
an `ANTHROPIC_API_KEY` for this endpoint. Get pay-as-you-go credit from
console.anthropic.com. The run below costs a few cents.

```bash
ANTHROPIC_API_KEY=sk-... go test -run TestPromptCachingAcrossTurns -v ./providers/anthropic/
```

Without the variable set, the test **skips** — CI stays green.

### Pointing it somewhere else

Two optional variables run the same test against a gateway, a regional
endpoint, or a local recording proxy:

| Variable | Meaning | Default |
|---|---|---|
| `ANTHROPIC_BASE_URL` | endpoint root, **without** `/v1/messages` | `https://api.anthropic.com` |
| `ANTHROPIC_MODEL` | model id | the client's `DefaultModel` |

```bash
ANTHROPIC_API_KEY=sk-... \
ANTHROPIC_BASE_URL=https://my-gateway.example.com \
ANTHROPIC_MODEL=claude-sonnet-5 \
  go test -run TestPromptCachingAcrossTurns -v ./providers/anthropic/
```

The test logs `endpoint=… model=…` before anything else, and both failure
messages repeat them — against a custom endpoint, a gateway that strips
`cache_control` or drops the usage fields is a far likelier explanation than a
defect in this client.

The model is not a cosmetic override. **The minimum cacheable prefix is
model-dependent**, so a smaller model may refuse to cache the prefix this test
builds. That surfaces as `cache_creation_input_tokens == 0` on turn 1 —
lengthen the prefix rather than relaxing the assertion. Per spec §3.1,
this test may be skipped in CI but must **not** be skipped in code-review
sign-off before a release tag.

What it asserts:

1. Two sequential calls share a byte-identical static prefix.
2. Turn 1 reports `cache_creation_input_tokens > 0` — the prefix was written to
   the cache.
3. Turn 2 reports `cache_read_input_tokens > 0` — the prefix was reused.

If turn 1 writes nothing, the prefix is below the provider's minimum cacheable
length; lengthen it rather than deleting the assertion.

This test makes two real, billed API calls. They are small (64 max output
tokens), but they are not free.
