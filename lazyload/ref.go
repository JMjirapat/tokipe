// Package lazyload defines the reference/resolver contract that lets an agent
// carry cheap handles in its prompt and pull the expensive bytes only when the
// model actually asks for them (docs/spec.md §2.4.5).
//
// NOT WIRED BY DEFAULT: lazyload is deliberately absent from every default
// pipeline. Using it changes the tool-exposure surface — the caller must
// expose a `load_content` tool to the model and route its invocations to a
// Loader — so it is opt-in per caller, never implicit.
package lazyload

import (
	"context"
	"errors"
)

// Ref is a cheap handle to content that has not been loaded yet.
type Ref struct {
	// ID identifies the content within the Loader's namespace. For FileLoader
	// this is a slash-separated path relative to the loader's root.
	ID string

	// Kind is "file" | "doc" | "chunk". An empty Kind is treated as "file" by
	// FileLoader; other loaders may choose their own default.
	Kind string
}

// Kind constants for Ref.Kind.
const (
	KindFile  = "file"
	KindDoc   = "doc"
	KindChunk = "chunk"
)

// Loader resolves a Ref to its content.
//
// Implementations must respect ctx cancellation and must treat every Ref as
// untrusted input: a Ref may have been echoed back by a model and is therefore
// attacker-influenced whenever any prompt content is.
type Loader interface {
	Resolve(ctx context.Context, ref Ref) (string, error)
}

// Sentinel errors returned by FileLoader. Callers should match with
// errors.Is rather than string comparison.
var (
	// ErrEmptyRef means Ref.ID was empty or whitespace.
	ErrEmptyRef = errors.New("lazyload: empty ref id")

	// ErrUnsupportedKind means the Loader does not serve this Ref.Kind.
	ErrUnsupportedKind = errors.New("lazyload: unsupported ref kind")

	// ErrPathEscape means the Ref resolved outside the loader's root. This is
	// a security failure, not a not-found: it is reported distinctly and never
	// downgraded to a silent empty result.
	ErrPathEscape = errors.New("lazyload: ref resolves outside root")

	// ErrNotRegularFile means the Ref resolved to a directory, device, socket
	// or similar.
	ErrNotRegularFile = errors.New("lazyload: not a regular file")

	// ErrTooLarge means the target exceeded the loader's MaxBytes.
	ErrTooLarge = errors.New("lazyload: file exceeds max size")
)
