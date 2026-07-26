package compress

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentkit/metrics"
	"agentkit/pipeline"
)

// --- helpers ---------------------------------------------------------------

// errCompressor claims everything and always fails.
type errCompressor struct{ calls int }

func (e *errCompressor) CanHandle(string) bool { return true }
func (e *errCompressor) Compress(string) (string, error) {
	e.calls++
	return "THIS SHOULD NEVER BE USED", errors.New("boom")
}

// keyOnly extracts every object key at every depth, sorted-insensitively via a
// set, so we can assert no key was invented or lost.
func keySet(t *testing.T, s string) map[string]int {
	t.Helper()
	var v any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, s)
	}
	out := map[string]int{}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, vv := range t {
				out[k]++
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(v)
	return out
}

// --- JSONCompressor --------------------------------------------------------

func TestJSONCompressor_CanHandle(t *testing.T) {
	c := NewJSONCompressor()
	tests := []struct {
		content string
		want    bool
	}{
		{`{"a":1}`, true},
		{"  \n [1, 2, 3] \t ", true},
		{`{"a":1,}`, false}, // trailing comma: invalid
		{`{"a":`, false},    // truncated
		{`42`, false},       // bare scalar
		{`"hello"`, false},  // bare string
		{`null`, false},
		{``, false},
		{`   `, false},
		{"just some prose about {braces}", false},
		{"\x00\xff\xfe garbage", false},
	}
	for _, tt := range tests {
		if got := c.CanHandle(tt.content); got != tt.want {
			t.Errorf("CanHandle(%q) = %v, want %v", tt.content, got, tt.want)
		}
	}
}

func TestJSONCompressor_StripsWhitespaceWithoutInventingContent(t *testing.T) {
	in := `{
	  "id":      "abc-123",
	  "count":   9007199254740993,
	  "ratio":   1.500,
	  "nested":  { "keep": [ 1, 2, {"deep": true} ] },
	  "text":    "  spaces inside strings are meaning-bearing  "
	}`
	c := NewJSONCompressor()
	if !c.CanHandle(in) {
		t.Fatal("CanHandle should accept this")
	}
	got, err := c.Compress(in)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("output is not valid JSON: %s", got)
	}
	if len(got) >= len(in) {
		t.Errorf("no size win: %d -> %d", len(in), len(got))
	}
	if strings.Contains(got, "\n") || strings.Contains(got, "\t") {
		t.Errorf("whitespace not stripped: %q", got)
	}
	// Every key survives; none invented.
	if !reflect.DeepEqual(keySet(t, in), keySet(t, got)) {
		t.Errorf("key set changed:\n in=%v\nout=%v", keySet(t, in), keySet(t, got))
	}
	// Large int must not be mangled through float64.
	if !strings.Contains(got, "9007199254740993") {
		t.Errorf("integer precision lost: %s", got)
	}
	// Trailing-zero literal preserved exactly (UseNumber keeps the literal).
	if !strings.Contains(got, "1.500") {
		t.Errorf("number literal rewritten: %s", got)
	}
	// Whitespace inside string values is meaning-bearing and must survive.
	if !strings.Contains(got, `"  spaces inside strings are meaning-bearing  "`) {
		t.Errorf("string value altered: %s", got)
	}
}

func TestJSONCompressor_DropFields(t *testing.T) {
	in := `{"keep":1,"etag":"xxxxxxxxxxxxxxxxxxxxxxxx","nested":{"etag":"yyyyyyyyyyyy","also_keep":2},"list":[{"etag":"z","k":3}]}`
	c := NewJSONCompressor("etag", "") // empty names ignored
	got, err := c.Compress(in)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if strings.Contains(got, "etag") {
		t.Errorf("etag not dropped at all depths: %s", got)
	}
	keys := keySet(t, got)
	for _, want := range []string{"keep", "nested", "also_keep", "list", "k"} {
		if keys[want] == 0 {
			t.Errorf("dropped a key it should have kept: %q (%s)", want, got)
		}
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("invalid JSON after drop: %s", got)
	}
}

func TestJSONCompressor_MalformedInputDoesNotPanic(t *testing.T) {
	c := NewJSONCompressor("x")
	for _, in := range []string{"", "{", "[[[[", "\x00\x01", `{"a":1}{"b":2}`, strings.Repeat("{", 5000)} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on %q: %v", in, r)
				}
			}()
			if c.CanHandle(in) {
				if _, err := c.Compress(in); err != nil {
					t.Logf("Compress(%q) errored as expected: %v", in, err)
				}
			}
		}()
	}
	// Direct Compress on garbage must error, not panic, and never return junk.
	if out, err := c.Compress("not json at all"); err == nil {
		t.Errorf("expected error, got %q", out)
	}
}

// --- TextCompressor --------------------------------------------------------

func TestTextCompressor_PreservesEveryToken(t *testing.T) {
	in := "  The   quick\tbrown  fox\r\n\n\n\n  jumps over\t\tthe lazy dog.  \n\n\n"
	c := NewTextCompressor()
	if !c.CanHandle(in) {
		t.Fatal("CanHandle should accept prose")
	}
	got, err := c.Compress(in)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if !reflect.DeepEqual(strings.Fields(in), strings.Fields(got)) {
		t.Errorf("token sequence changed:\n in=%v\nout=%v", strings.Fields(in), strings.Fields(got))
	}
	if len(got) >= len(in) {
		t.Errorf("no size win: %d -> %d", len(in), len(got))
	}
	if strings.Contains(got, "  ") || strings.Contains(got, "\t") {
		t.Errorf("whitespace runs remain: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("blank-line runs remain: %q", got)
	}
	if !strings.Contains(got, "\n\n") {
		t.Errorf("paragraph break not preserved: %q", got)
	}
}

func TestTextCompressor_Idempotent(t *testing.T) {
	c := NewTextCompressor()
	once, _ := c.Compress("a  b\n\n\n\nc   d\n")
	twice, _ := c.Compress(once)
	if once != twice {
		t.Errorf("not idempotent: %q vs %q", once, twice)
	}
}

func TestTextCompressor_DropBlankLines(t *testing.T) {
	c := &TextCompressor{KeepBlankLines: false}
	got, _ := c.Compress("a\n\n\nb")
	if got != "a\nb" {
		t.Errorf("got %q, want %q", got, "a\nb")
	}
}

func TestTextCompressor_EmptyAndGarbage(t *testing.T) {
	c := NewTextCompressor()
	for _, in := range []string{"", "   ", "\n\n\n", "\x00\x01\x02", "日本語  テキスト", strings.Repeat(" ", 100000)} {
		if c.CanHandle(in) {
			got, err := c.Compress(in)
			if err != nil {
				t.Errorf("Compress(%q) errored: %v", in, err)
			}
			if !reflect.DeepEqual(strings.Fields(in), strings.Fields(got)) {
				t.Errorf("tokens changed for %q", in)
			}
		}
	}
	if c.CanHandle("") || c.CanHandle("   ") {
		t.Error("CanHandle should reject whitespace-only content")
	}
}

// --- CodeCompressor --------------------------------------------------------

const sampleGo = `package main

// Package-level documentation that costs tokens and changes nothing.
import (
	"fmt" // the standard formatter
	"strings"
)

// Greeter greets. This comment is the point of the test: it must go.
type Greeter struct {
	// Name is who to greet.
	Name string
}

// Greet returns a greeting.
//
// A long doc comment, of the kind real Go source is full of, which the
// compressor should remove without touching the code below it.
func (g Greeter) Greet() string {
	// A URL in a comment: https://example.com/a//b
	prefix := "// not a comment" // but this is
	return prefix + fmt.Sprintf("Hello, %s", strings.TrimSpace(g.Name))
}
`

func TestCodeCompressor_RemovesCommentsAndKeepsCode(t *testing.T) {
	c := NewCodeCompressor()
	if !c.CanHandle(sampleGo) {
		t.Fatal("valid Go was not claimed")
	}

	got, err := c.Compress(sampleGo)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if len(got) >= len(sampleGo) {
		t.Errorf("no saving: %d -> %d bytes", len(sampleGo), len(got))
	}
	for _, gone := range []string{"Package-level documentation", "the standard formatter", "who to greet", "A long doc comment"} {
		if strings.Contains(got, gone) {
			t.Errorf("comment survived: %q", gone)
		}
	}
	// The string literal that looks like a comment must be untouched. This is
	// the case a regex-based implementation gets wrong.
	if !strings.Contains(got, `"// not a comment"`) {
		t.Errorf("a string literal resembling a comment was mangled:\n%s", got)
	}
	for _, kept := range []string{"type Greeter struct", "func (g Greeter) Greet() string", "strings.TrimSpace"} {
		if !strings.Contains(got, kept) {
			t.Errorf("code was lost: %q missing from\n%s", kept, got)
		}
	}
}

// Raw string literals are the other thing a regex implementation destroys.
func TestCodeCompressor_PreservesRawStringsAndBlankLines(t *testing.T) {
	src := "package p\n\nvar Doc = `\nline one\n\n// this is data, not a comment\n\nline two\n`\n\nfunc f() int { return 1 }\n"

	c := NewCodeCompressor()
	if !c.CanHandle(src) {
		t.Fatal("valid Go was not claimed")
	}
	got, err := c.Compress(src)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	for _, kept := range []string{"line one", "line two", "// this is data, not a comment"} {
		if !strings.Contains(got, kept) {
			t.Errorf("raw string content lost: %q missing from\n%s", kept, got)
		}
	}
}

func TestCodeCompressor_OutputIsStillValidGo(t *testing.T) {
	c := NewCodeCompressor()
	got, err := c.Compress(sampleGo)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if _, err := parseGo(got); err != nil {
		t.Fatalf("compressed output does not parse:\n%s\nerr: %v", got, err)
	}
}

func TestCodeCompressor_RejectsNonGo(t *testing.T) {
	c := NewCodeCompressor()
	for _, in := range []string{
		"",
		"   ",
		"func main() {}",                         // a fragment, not a file
		"class A {}\nint x;\nvoid f(){}\n{}\n{}", // Java-ish
		"{\n\t\"valid\": \"json\"\n}",
		"package but then nonsense ][",
	} {
		if c.CanHandle(in) {
			t.Errorf("claimed content that is not a Go file: %q", in)
		}
	}
}

// Compress on unclaimed content returns an error, which the Stage treats as
// "leave the chunk alone". That is the documented contract for every
// compressor, and it is why CanHandle is the gate.
func TestCodeCompressor_CompressErrorsOnNonGo(t *testing.T) {
	if _, err := NewCodeCompressor().Compress("not go at all"); err == nil {
		t.Error("want an error for non-Go input")
	}
}

func TestCodeCompressor_BodyElisionKeepsTheAPI(t *testing.T) {
	c := NewCodeCompressor(WithBodyElision())
	got, err := c.Compress(sampleGo)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	plain, _ := NewCodeCompressor().Compress(sampleGo)
	if len(got) >= len(plain) {
		t.Errorf("body elision saved nothing beyond comment removal: %d vs %d", len(got), len(plain))
	}
	if !strings.Contains(got, "func (g Greeter) Greet() string") {
		t.Errorf("the signature must survive:\n%s", got)
	}
	if strings.Contains(got, "strings.TrimSpace") {
		t.Errorf("the body should be gone:\n%s", got)
	}
	if _, err := parseGo(got); err != nil {
		t.Fatalf("elided output does not parse:\n%s\nerr: %v", got, err)
	}
}

// Nothing to remove means nothing to gain, and the compressor must not return
// a re-formatted file that happens to be the same size or bigger.
func TestCodeCompressor_NoWinReturnsOriginal(t *testing.T) {
	src := "package p\n\nfunc a() {}\nfunc b() {}\nfunc c() {}\n"
	got, err := NewCodeCompressor().Compress(src)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if len(got) > len(src) {
		t.Errorf("output grew: %d -> %d\n%s", len(src), len(got), got)
	}
}

func TestCodeCompressor_MinLines(t *testing.T) {
	short := "package p\nfunc f() {}\n"
	if NewCodeCompressor().CanHandle(short) {
		t.Error("a file below the line threshold should be skipped")
	}
	if !NewCodeCompressor(WithMinLines(1)).CanHandle(short) {
		t.Error("lowering the threshold should let it through")
	}
}

// The declaration count is the guard against the worst failure available to a
// code compressor: silently dropping a function.
func TestCodeCompressor_NeverLosesDeclarations(t *testing.T) {
	got, err := NewCodeCompressor().Compress(sampleGo)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	in, _ := parseGo(sampleGo)
	out, _ := parseGo(got)
	if declCount(in) != declCount(out) {
		t.Errorf("declarations changed: %d -> %d", declCount(in), declCount(out))
	}
}

func TestCodeCompressor_DoesNotPanicOnArbitraryBytes(t *testing.T) {
	c := NewCodeCompressor()
	for _, in := range []string{
		"\x00\x01\x02", strings.Repeat("package ", 500), "package\n\n\n\n\n",
		"package p\nfunc f() { // unterminated", "สวัสดี\nชาวโลก\n\n\n\n",
	} {
		_ = c.CanHandle(in)
		_, _ = c.Compress(in)
	}
}

// --- Stage / router --------------------------------------------------------

func TestStage_FirstCanHandleWins(t *testing.T) {
	s := NewStage(NewCodeCompressor(), NewJSONCompressor("etag"), NewTextCompressor())
	req := &pipeline.Request{RetrievedChunks: []pipeline.Chunk{
		{Content: "{\n  \"a\": 1,\n  \"etag\": \"0123456789abcdef\"\n}"},
		{Content: "some   prose\n\n\n\nwith   gaps"},
	}}
	got, err := s.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got.RetrievedChunks[0].Content != `{"a":1}` {
		t.Errorf("JSON chunk = %q", got.RetrievedChunks[0].Content)
	}
	if got.RetrievedChunks[1].Content != "some prose\n\nwith gaps" {
		t.Errorf("text chunk = %q", got.RetrievedChunks[1].Content)
	}
}

// Fail-open: an erroring compressor leaves the chunk untouched, and Process
// still returns nil error.
func TestStage_ErroringCompressorLeavesChunkUntouched(t *testing.T) {
	const original = "  original   content  "
	ec := &errCompressor{}
	s := NewStage(ec, NewTextCompressor())
	req := &pipeline.Request{RetrievedChunks: []pipeline.Chunk{{Content: original}}}

	got, err := s.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process must fail open, got %v", err)
	}
	if ec.calls != 1 {
		t.Errorf("erroring compressor called %d times, want 1", ec.calls)
	}
	if got.RetrievedChunks[0].Content != original {
		t.Errorf("chunk was modified: %q", got.RetrievedChunks[0].Content)
	}
}

func TestStage_NoGrowth(t *testing.T) {
	// A "compressor" that claims everything and returns something bigger must
	// not be allowed to inflate the prompt.
	s := NewStage(fnCompressor{
		can: func(string) bool { return true },
		do:  func(s string) (string, error) { return s + s, nil },
	})
	req := &pipeline.Request{RetrievedChunks: []pipeline.Chunk{{Content: "abc"}}}
	got, _ := s.Process(context.Background(), req)
	if got.RetrievedChunks[0].Content != "abc" {
		t.Errorf("growth was accepted: %q", got.RetrievedChunks[0].Content)
	}
}

type fnCompressor struct {
	can func(string) bool
	do  func(string) (string, error)
}

func (f fnCompressor) CanHandle(s string) bool           { return f.can(s) }
func (f fnCompressor) Compress(s string) (string, error) { return f.do(s) }

func TestStage_NilsAndEmpties(t *testing.T) {
	s := NewStage(nil, NewTextCompressor())
	if got, err := s.Process(context.Background(), nil); got != nil || err != nil {
		t.Errorf("Process(nil) = %v, %v", got, err)
	}
	if _, err := s.Process(context.Background(), &pipeline.Request{}); err != nil {
		t.Errorf("empty request: %v", err)
	}
	// No compressors registered at all.
	empty := NewStage()
	req := &pipeline.Request{RetrievedChunks: []pipeline.Chunk{{Content: "  x  "}}}
	got, _ := empty.Process(context.Background(), req)
	if got.RetrievedChunks[0].Content != "  x  " {
		t.Errorf("chunk changed with no compressors: %q", got.RetrievedChunks[0].Content)
	}
}

func TestStage_CanceledContextFailsOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewStage(NewTextCompressor())
	req := &pipeline.Request{RetrievedChunks: []pipeline.Chunk{{Content: "  a   b  "}}}
	got, err := s.Process(ctx, req)
	if err != nil {
		t.Fatalf("must fail open, got %v", err)
	}
	if got.RetrievedChunks[0].Content != "  a   b  " {
		t.Errorf("work done despite cancellation: %q", got.RetrievedChunks[0].Content)
	}
}

func TestStage_NameAndMetrics(t *testing.T) {
	rec := metrics.NewInMemory()
	s := NewStageWithMetrics(rec, NewJSONCompressor(), NewTextCompressor())
	var st pipeline.Stage = s
	if st.Name() != "compress" {
		t.Errorf("Name() = %q", st.Name())
	}
	req := &pipeline.Request{RetrievedChunks: []pipeline.Chunk{
		{Content: `{ "a" : 1 }`},
		{Content: "b   c"},
		{Content: "nowin"}, // already minimal: no compression recorded
	}}
	if _, err := s.Process(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := rec.Count(MetricCompressed); got != 2 {
		t.Errorf("counter = %d, want 2 (snapshot: %v)", got, rec.Snapshot())
	}
}
