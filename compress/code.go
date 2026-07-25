package compress

// CodeCompressor is a PLACEHOLDER. It compresses nothing.
//
// CanHandle always returns false, so the Stage's content router skips it
// entirely and the chunk falls through to the next registered compressor
// (normally TextCompressor). Registering it today is harmless and lets callers
// pin the intended ordering — specific-before-catch-all — before the real
// implementation lands.
//
// Phase 2+ work (docs/spec.md §2.4.4, Implementation Plan §2.4): a real
// implementation must be AST-aware, not regex-based. Stripping comments and
// blank lines with regexes mangles string literals and raw strings, which for
// code is a correctness bug, not a cosmetic one. The planned approach is
// per-language parsers (go/parser first, since it is stdlib and the core
// module is stdlib-only) producing a signature-level skeleton: package/import
// lines, type and function signatures, doc comments, function bodies elided.
// Anything that cannot be parsed must fall back to CanHandle == false, exactly
// as this stub does.
type CodeCompressor struct{}

// NewCodeCompressor returns the v1 no-op CodeCompressor.
func NewCodeCompressor() *CodeCompressor { return &CodeCompressor{} }

// CanHandle always returns false in v1. See the type doc.
func (c *CodeCompressor) CanHandle(string) bool { return false }

// Compress is unreachable via Stage (CanHandle is always false) and returns
// content unchanged so that a caller invoking it directly still gets
// meaning-preserving behaviour rather than an error or empty string.
func (c *CodeCompressor) Compress(content string) (string, error) { return content, nil }

var _ Compressor = (*CodeCompressor)(nil)
