package lazyload

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"context"
)

// DefaultMaxBytes caps a single Resolve. Lazy loading exists to keep prompts
// small; silently inlining a 500 MB file would defeat the purpose and blow up
// memory, so there is always a limit.
const DefaultMaxBytes int64 = 4 << 20 // 4 MiB

// FileLoader resolves Refs against a fixed root directory on local disk.
//
// SECURITY MODEL: the root is a hard boundary. A Ref is untrusted input —
// typically it round-trips through the model, so a prompt injection can put
// arbitrary strings in it. Three independent checks enforce containment, and
// all three must pass:
//
//  1. Syntactic rejection — absolute paths, Windows volume/UNC prefixes, and
//     any ".." path segment are refused outright, before any filesystem call.
//  2. Lexical containment — the cleaned, absolute join of root and ref is
//     verified to be inside root with filepath.Rel. This catches anything the
//     segment scan missed (encodings, redundant separators).
//  3. Physical containment — filepath.EvalSymlinks resolves the real on-disk
//     path (following symlinks in every path component, not just the last)
//     and containment is re-verified. This is the check that stops a symlink
//     *inside* the root from pointing at /etc/passwd. os.Lstat additionally
//     identifies the final component so directories, devices and dangling
//     links are rejected with a precise error.
//
// A FileLoader is immutable after construction and safe for concurrent use.
type FileLoader struct {
	root     string // absolute, cleaned, symlink-resolved
	maxBytes int64
}

// FileOption configures a FileLoader.
type FileOption func(*FileLoader)

// WithMaxBytes overrides DefaultMaxBytes. A value <= 0 restores the default.
func WithMaxBytes(n int64) FileOption {
	return func(l *FileLoader) {
		if n <= 0 {
			n = DefaultMaxBytes
		}
		l.maxBytes = n
	}
}

// NewFileLoader builds a FileLoader rooted at root.
//
// root is resolved to an absolute, symlink-free path at construction time so
// that later containment comparisons are exact. This matters on macOS, where
// /tmp is itself a symlink to /private/tmp: without resolving the root up
// front, every legitimate read under /tmp would look like an escape.
//
// It is an error for root to be empty or not to be an existing directory —
// that is a programming/configuration error, and the fail-open rule does not
// apply to a misconfigured required dependency (docs/spec.md §2.5 rule 1).
func NewFileLoader(root string, opts ...FileOption) (*FileLoader, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("lazyload: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("lazyload: resolving root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("lazyload: resolving root: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("lazyload: resolving root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("lazyload: root %q is not a directory", root)
	}

	l := &FileLoader{root: filepath.Clean(real), maxBytes: DefaultMaxBytes}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	return l, nil
}

// Root returns the resolved absolute root directory.
func (l *FileLoader) Root() string { return l.root }

// Resolve reads the file named by ref.ID relative to the loader's root.
//
// Cancellation is checked before any filesystem access, so a canceled context
// guarantees no read took place.
func (l *FileLoader) Resolve(ctx context.Context, ref Ref) (string, error) {
	// Check cancellation FIRST: the contract is "canceled ctx never reads".
	if ctx == nil {
		return "", fmt.Errorf("lazyload: nil context")
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("lazyload: %w", err)
	}

	if ref.Kind != "" && ref.Kind != KindFile {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedKind, ref.Kind)
	}

	path, err := l.safePath(ref.ID)
	if err != nil {
		return "", err
	}

	// Re-check cancellation right before touching the disk.
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("lazyload: %w", err)
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("lazyload: opening %q: %w", ref.ID, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("lazyload: stat %q: %w", ref.ID, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %q", ErrNotRegularFile, ref.ID)
	}
	if info.Size() > l.maxBytes {
		return "", fmt.Errorf("%w: %q is %d bytes (max %d)", ErrTooLarge, ref.ID, info.Size(), l.maxBytes)
	}

	// LimitReader guards against the file growing between Stat and Read.
	b, err := io.ReadAll(io.LimitReader(f, l.maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("lazyload: reading %q: %w", ref.ID, err)
	}
	if int64(len(b)) > l.maxBytes {
		return "", fmt.Errorf("%w: %q", ErrTooLarge, ref.ID)
	}
	return string(b), nil
}

// safePath applies all three containment checks and returns the absolute
// on-disk path to read.
func (l *FileLoader) safePath(id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", ErrEmptyRef
	}
	// NUL bytes truncate paths in some syscalls; refuse them outright.
	if strings.ContainsRune(id, '\x00') {
		return "", fmt.Errorf("%w: NUL byte in ref id", ErrPathEscape)
	}

	// Refs use forward slashes regardless of host OS. A backslash is therefore
	// never a separator here — and on Unix it would be a legal *filename*
	// character, so `..\..\etc\passwd` would silently become one weird
	// filename instead of a traversal. Refusing backslashes outright keeps the
	// syntactic check below meaningful on every platform.
	if strings.ContainsRune(id, '\\') {
		return "", fmt.Errorf("%w: backslash in ref id %q (refs use forward slashes)", ErrPathEscape, id)
	}
	rel := filepath.FromSlash(id)

	// --- check 1: syntactic ------------------------------------------------
	if filepath.IsAbs(rel) || strings.HasPrefix(id, "/") {
		return "", fmt.Errorf("%w: absolute path %q", ErrPathEscape, id)
	}
	if vol := filepath.VolumeName(rel); vol != "" {
		return "", fmt.Errorf("%w: volume-qualified path %q", ErrPathEscape, id)
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: %q contains a %q segment", ErrPathEscape, id, "..")
		}
	}

	// --- check 2: lexical --------------------------------------------------
	joined := filepath.Clean(filepath.Join(l.root, rel))
	if !within(l.root, joined) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, id)
	}

	// --- check 3: physical (symlinks) --------------------------------------
	// Lstat first so a symlink is identified without following it. Its absence
	// is a plain not-found, reported as such.
	if _, err := os.Lstat(joined); err != nil {
		return "", fmt.Errorf("lazyload: %q: %w", id, err)
	}
	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		// A dangling or looping symlink lands here. Treat it as an escape:
		// we cannot prove containment, so we refuse.
		return "", fmt.Errorf("%w: %q: %v", ErrPathEscape, id, err)
	}
	if !within(l.root, filepath.Clean(real)) {
		return "", fmt.Errorf("%w: %q resolves to %q", ErrPathEscape, id, real)
	}
	return real, nil
}

// within reports whether path is root itself or lies beneath it. Both
// arguments must already be absolute and cleaned.
func within(root, path string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

var _ Loader = (*FileLoader)(nil)
