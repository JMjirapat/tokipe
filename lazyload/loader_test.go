package lazyload

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newRoot builds a temp tree:
//
//	<root>/doc.txt
//	<root>/sub/nested.txt
//	<root>/emptydir/
//
// and a sibling <outside>/secret.txt that must never be reachable.
func newRoot(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(root, "sub"), filepath.Join(root, "emptydir")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "doc.txt"), "hello lazyload")
	write(t, filepath.Join(root, "sub", "nested.txt"), "nested content")
	write(t, filepath.Join(outside, "secret.txt"), "TOP SECRET")
	return root, outside
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustLoader(t *testing.T, root string, opts ...FileOption) *FileLoader {
	t.Helper()
	l, err := NewFileLoader(root, opts...)
	if err != nil {
		t.Fatalf("NewFileLoader: %v", err)
	}
	return l
}

// --- happy path ------------------------------------------------------------

func TestResolve_HappyPath(t *testing.T) {
	root, _ := newRoot(t)
	l := mustLoader(t, root)

	tests := []struct {
		id, want string
		kind     string
	}{
		{"doc.txt", "hello lazyload", ""},
		{"doc.txt", "hello lazyload", KindFile},
		{"sub/nested.txt", "nested content", ""},
		{"./doc.txt", "hello lazyload", ""},
		{"sub/../doc.txt", "", ""}, // ".." rejected even when it stays inside
	}
	for _, tt := range tests {
		got, err := l.Resolve(context.Background(), Ref{ID: tt.id, Kind: tt.kind})
		if tt.want == "" {
			if err == nil {
				t.Errorf("Resolve(%q) unexpectedly succeeded", tt.id)
			}
			continue
		}
		if err != nil {
			t.Errorf("Resolve(%q): %v", tt.id, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Resolve(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestRoot(t *testing.T) {
	root, _ := newRoot(t)
	l := mustLoader(t, root)
	if !filepath.IsAbs(l.Root()) {
		t.Errorf("Root() = %q, want absolute", l.Root())
	}
}

// --- traversal -------------------------------------------------------------

func TestResolve_RejectsTraversal(t *testing.T) {
	root, outside := newRoot(t)
	l := mustLoader(t, root)

	ids := []string{
		"../../etc/passwd",
		"../outside/secret.txt",
		"../../../../../../../../etc/passwd",
		"sub/../../outside/secret.txt",
		"..",
		"../",
		"sub/..",
		"a/b/../../../outside/secret.txt",
		`..\..\windows\system32`,
	}
	for _, id := range ids {
		got, err := l.Resolve(context.Background(), Ref{ID: id})
		if err == nil {
			t.Errorf("Resolve(%q) succeeded and returned %q — traversal not blocked", id, got)
			continue
		}
		if !errors.Is(err, ErrPathEscape) {
			t.Errorf("Resolve(%q) error = %v, want ErrPathEscape", id, err)
		}
		if strings.Contains(got, "TOP SECRET") {
			t.Fatalf("Resolve(%q) leaked content from %s", id, outside)
		}
	}
}

func TestResolve_RejectsAbsolutePaths(t *testing.T) {
	root, outside := newRoot(t)
	l := mustLoader(t, root)

	ids := []string{
		"/etc/passwd",
		filepath.Join(outside, "secret.txt"),
		filepath.Join(root, "doc.txt"), // absolute even though it is inside
		`\windows\system32\config`,
	}
	for _, id := range ids {
		_, err := l.Resolve(context.Background(), Ref{ID: id})
		if err == nil {
			t.Errorf("Resolve(%q) succeeded — absolute path not blocked", id)
			continue
		}
		if !errors.Is(err, ErrPathEscape) {
			t.Errorf("Resolve(%q) error = %v, want ErrPathEscape", id, err)
		}
	}
}

func TestResolve_RejectsEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	root, outside := newRoot(t)

	// A symlink INSIDE the root pointing at a file OUTSIDE it: lexically
	// contained, physically an escape.
	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
		t.Fatal(err)
	}
	// A symlinked DIRECTORY inside the root pointing outside.
	dirLink := filepath.Join(root, "escapedir")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatal(err)
	}
	// A benign symlink that stays inside the root must still work.
	if err := os.Symlink(filepath.Join(root, "doc.txt"), filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink.
	if err := os.Symlink(filepath.Join(outside, "nope.txt"), filepath.Join(root, "dangling.txt")); err != nil {
		t.Fatal(err)
	}

	l := mustLoader(t, root)

	for _, id := range []string{"escape.txt", "escapedir/secret.txt", "dangling.txt"} {
		got, err := l.Resolve(context.Background(), Ref{ID: id})
		if err == nil {
			t.Errorf("Resolve(%q) succeeded with %q — symlink escape not blocked", id, got)
			continue
		}
		if !errors.Is(err, ErrPathEscape) {
			t.Errorf("Resolve(%q) error = %v, want ErrPathEscape", id, err)
		}
		if strings.Contains(got, "TOP SECRET") {
			t.Fatalf("Resolve(%q) leaked secret content", id)
		}
	}

	if got, err := l.Resolve(context.Background(), Ref{ID: "alias.txt"}); err != nil || got != "hello lazyload" {
		t.Errorf("in-root symlink should resolve: got %q, %v", got, err)
	}
}

// --- context ---------------------------------------------------------------

func TestResolve_CanceledContext(t *testing.T) {
	root, _ := newRoot(t)
	l := mustLoader(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := l.Resolve(ctx, Ref{ID: "doc.txt"})
	if err == nil {
		t.Fatalf("canceled context must error, got %q", got)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if got != "" {
		t.Errorf("returned content %q despite cancellation — a read happened", got)
	}

	if _, err := l.Resolve(nil, Ref{ID: "doc.txt"}); err == nil { //nolint:staticcheck // nil ctx is deliberately tested
		t.Error("nil context should error")
	}
}

// --- other validation ------------------------------------------------------

func TestResolve_Validation(t *testing.T) {
	root, _ := newRoot(t)
	l := mustLoader(t, root)

	tests := []struct {
		name    string
		ref     Ref
		wantErr error
	}{
		{"empty id", Ref{ID: ""}, ErrEmptyRef},
		{"whitespace id", Ref{ID: "   "}, ErrEmptyRef},
		{"nul byte", Ref{ID: "doc.txt\x00.png"}, ErrPathEscape},
		{"unsupported kind", Ref{ID: "doc.txt", Kind: KindDoc}, ErrUnsupportedKind},
		{"chunk kind", Ref{ID: "doc.txt", Kind: KindChunk}, ErrUnsupportedKind},
		{"directory", Ref{ID: "emptydir"}, ErrNotRegularFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := l.Resolve(context.Background(), tt.ref)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}

	if _, err := l.Resolve(context.Background(), Ref{ID: "missing.txt"}); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file error = %v, want os.ErrNotExist", err)
	}
}

func TestResolve_MaxBytes(t *testing.T) {
	root, _ := newRoot(t)
	write(t, filepath.Join(root, "big.txt"), strings.Repeat("x", 100))

	l := mustLoader(t, root, WithMaxBytes(10))
	if _, err := l.Resolve(context.Background(), Ref{ID: "big.txt"}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("error = %v, want ErrTooLarge", err)
	}

	// Non-positive limit restores the default.
	l2 := mustLoader(t, root, WithMaxBytes(0), nil)
	if l2.maxBytes != DefaultMaxBytes {
		t.Errorf("maxBytes = %d, want %d", l2.maxBytes, DefaultMaxBytes)
	}
	if got, err := l2.Resolve(context.Background(), Ref{ID: "big.txt"}); err != nil || len(got) != 100 {
		t.Errorf("got %d bytes, %v", len(got), err)
	}
}

func TestNewFileLoader_Errors(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "afile"), "x")

	for _, root := range []string{"", "   ", filepath.Join(base, "does-not-exist"), filepath.Join(base, "afile")} {
		if _, err := NewFileLoader(root); err == nil {
			t.Errorf("NewFileLoader(%q) succeeded, want error", root)
		}
	}
}

// The root itself may be reached through a symlink (e.g. macOS /tmp ->
// /private/tmp) without every read looking like an escape.
func TestNewFileLoader_SymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(real, "doc.txt"), "via symlinked root")

	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	l := mustLoader(t, link)
	got, err := l.Resolve(context.Background(), Ref{ID: "doc.txt"})
	if err != nil || got != "via symlinked root" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestWithin(t *testing.T) {
	root := filepath.Clean("/a/b")
	tests := []struct {
		path string
		want bool
	}{
		{"/a/b", true},
		{"/a/b/c", true},
		{"/a/b/c/d", true},
		{"/a", false},
		{"/a/bc", false},
		{"/", false},
		{"/x/y", false},
	}
	for _, tt := range tests {
		if got := within(root, filepath.Clean(tt.path)); got != tt.want {
			t.Errorf("within(%q, %q) = %v, want %v", root, tt.path, got, tt.want)
		}
	}
}

func TestLoaderIsConcurrencySafe(t *testing.T) {
	root, _ := newRoot(t)
	l := mustLoader(t, root)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 25; j++ {
				if _, err := l.Resolve(context.Background(), Ref{ID: "doc.txt"}); err != nil {
					t.Error(err)
				}
				if _, err := l.Resolve(context.Background(), Ref{ID: "../../etc/passwd"}); err == nil {
					t.Error("traversal succeeded under concurrency")
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

var _ Loader = (*FileLoader)(nil)
