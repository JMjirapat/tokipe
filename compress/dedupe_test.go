package compress_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/JMjirapat/tokipe/compress"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
)

func chunks(contents ...string) []pipeline.Chunk {
	out := make([]pipeline.Chunk, len(contents))
	for i, c := range contents {
		out[i] = pipeline.Chunk{Content: c, SourceURL: fmt.Sprintf("src/%d", i)}
	}
	return out
}

func run(t *testing.T, st *compress.DedupeStage, cs []pipeline.Chunk) *pipeline.Request {
	t.Helper()
	req := &pipeline.Request{RetrievedChunks: cs}
	out, err := st.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return out
}

const passage = "Prompt caching anchors a breakpoint after static content only. " +
	"Dynamic content such as retrieved chunks must never sit before a breakpoint, " +
	"because it changes every turn and invalidates the cached prefix entirely."

func TestDropsExactDuplicates(t *testing.T) {
	rec := metrics.NewInMemory()
	st := compress.NewDedupeStage(compress.WithDedupeMetrics(rec))

	out := run(t, st, chunks(passage, passage, passage))
	if len(out.RetrievedChunks) != 1 {
		t.Fatalf("kept %d chunks, want 1", len(out.RetrievedChunks))
	}
	if got := out.Metadata[compress.MetaDeduped]; got != 2 {
		t.Errorf("MetaDeduped = %v, want 2", got)
	}
	if rec.Count(compress.MetricChunksDeduped) != 2 {
		t.Errorf("counter = %d, want 2", rec.Count(compress.MetricChunksDeduped))
	}
}

// The realistic case: the same passage reformatted, requoted, lightly edited.
func TestDropsNearDuplicates(t *testing.T) {
	reformatted := strings.ReplaceAll(passage, " ", "\n")
	requoted := "> " + strings.ReplaceAll(passage, ". ", ".\n> ")

	out := run(t, compress.NewDedupeStage(), chunks(passage, reformatted, requoted))
	if len(out.RetrievedChunks) != 1 {
		t.Fatalf("kept %d chunks, want 1:\n%+v", len(out.RetrievedChunks), out.RetrievedChunks)
	}
}

func TestKeepsGenuinelyDifferentChunks(t *testing.T) {
	other := "Tool results are keyed by a SHA-256 hash of the tool name and its " +
		"arguments. Argument maps are canonicalised first, so key order never " +
		"changes the resulting hash value at all."
	third := "Every optimization fails open. A compression error, a dead cache " +
		"backend, or an embedding timeout leaves the turn intact and simply " +
		"forfeits that particular optimization entirely."

	out := run(t, compress.NewDedupeStage(), chunks(passage, other, third))
	if len(out.RetrievedChunks) != 3 {
		t.Fatalf("kept %d chunks, want all 3 — different content was treated as duplicate", len(out.RetrievedChunks))
	}
}

// Word-order changes must not read as duplicates. This is why shingles are used
// rather than bare word sets, which would score these two identically.
func TestWordOrderMatters(t *testing.T) {
	a := "cache the prefix and never the suffix because the prefix is what the provider matches on every turn"
	b := "cache the suffix and never the prefix because the suffix is what the provider matches on every turn"

	out := run(t, compress.NewDedupeStage(), chunks(a, b))
	if len(out.RetrievedChunks) != 2 {
		t.Errorf("kept %d, want 2: reordered words must not count as duplicates", len(out.RetrievedChunks))
	}
}

// RAGStage orders chunks by relevance, so the first copy is the most relevant.
// Keeping the first preserves that ordering rather than silently re-ranking.
func TestKeepsTheFirstOccurrence(t *testing.T) {
	first := pipeline.Chunk{Content: passage, SourceURL: "canonical", Similarity: 0.95}
	second := pipeline.Chunk{Content: passage, SourceURL: "copy", Similarity: 0.40}

	out := run(t, compress.NewDedupeStage(), []pipeline.Chunk{first, second})
	if len(out.RetrievedChunks) != 1 {
		t.Fatalf("kept %d, want 1", len(out.RetrievedChunks))
	}
	if out.RetrievedChunks[0].SourceURL != "canonical" {
		t.Errorf("kept %q, want the first (most relevant) copy", out.RetrievedChunks[0].SourceURL)
	}
}

// A chunk too short to judge is kept. Dropping content the model needed is a
// worse failure than sending a duplicate.
func TestShortChunksAreAlwaysKept(t *testing.T) {
	out := run(t, compress.NewDedupeStage(), chunks("yes", "yes", "yes"))
	if len(out.RetrievedChunks) != 3 {
		t.Errorf("kept %d, want all 3 — chunks below the word floor must not be judged", len(out.RetrievedChunks))
	}
}

func TestThresholdIsConfigurable(t *testing.T) {
	a := passage
	b := passage + " An extra sentence that makes this chunk somewhat different from the first one."

	strict := run(t, compress.NewDedupeStage(compress.WithDedupeThreshold(0.99)), chunks(a, b))
	if len(strict.RetrievedChunks) != 2 {
		t.Errorf("a strict threshold kept %d, want 2", len(strict.RetrievedChunks))
	}
	loose := run(t, compress.NewDedupeStage(compress.WithDedupeThreshold(0.4)), chunks(a, b))
	if len(loose.RetrievedChunks) != 1 {
		t.Errorf("a loose threshold kept %d, want 1", len(loose.RetrievedChunks))
	}
}

func TestInvalidOptionsFallBackToDefaults(t *testing.T) {
	// A threshold of 0 or 2 would make the stage drop everything or nothing;
	// neither is a reasonable reading of the caller's intent.
	for _, bad := range []float64{0, -1, 1.5} {
		st := compress.NewDedupeStage(compress.WithDedupeThreshold(bad))
		out := run(t, st, chunks(passage, passage))
		if len(out.RetrievedChunks) != 1 {
			t.Errorf("threshold %v: kept %d, want default behaviour (1)", bad, len(out.RetrievedChunks))
		}
	}
	if st := compress.NewDedupeStage(compress.WithShingleSize(0)); st == nil {
		t.Error("an invalid shingle size should not produce a nil stage")
	}
}

func TestNoDuplicatesLeavesTheRequestUntouched(t *testing.T) {
	req := &pipeline.Request{RetrievedChunks: chunks(passage, "totally different content here that shares no shingles with the other one at all")}
	out, err := compress.NewDedupeStage().Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if out != req {
		t.Error("with nothing to drop the request should be returned as-is")
	}
	if _, ok := out.Metadata[compress.MetaDeduped]; ok {
		t.Error("MetaDeduped should not be set when nothing was dropped")
	}
}

func TestEdgeCasesDoNotPanic(t *testing.T) {
	st := compress.NewDedupeStage()
	cases := map[string]*pipeline.Request{
		"nil request":  nil,
		"no chunks":    {},
		"one chunk":    {RetrievedChunks: chunks(passage)},
		"empty chunks": {RetrievedChunks: chunks("", "", "")},
		"unicode":      {RetrievedChunks: chunks(strings.Repeat("สวัสดีชาวโลก ", 20), strings.Repeat("สวัสดีชาวโลก ", 20))},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := st.Process(context.Background(), req); err != nil {
				t.Errorf("Process: %v", err)
			}
		})
	}
}

func TestCanceledContextLeavesChunksAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := &pipeline.Request{RetrievedChunks: chunks(passage, passage)}
	out, err := compress.NewDedupeStage().Process(ctx, req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(out.RetrievedChunks) != 2 {
		t.Errorf("kept %d; a canceled context should skip the work, not do it", len(out.RetrievedChunks))
	}
}

func TestImplementsPipelineStage(t *testing.T) {
	var s pipeline.Stage = compress.NewDedupeStage()
	if s.Name() != "dedupe" {
		t.Errorf("Name = %q", s.Name())
	}
}
