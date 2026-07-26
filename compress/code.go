package compress

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
)

// CodeCompressor shrinks Go source using go/ast.
//
// It is AST-aware rather than regex-based, and for code that distinction is a
// correctness question rather than a stylistic one: a regex stripping "// …"
// also mutilates a URL inside a string literal, and one stripping blank lines
// corrupts raw string literals. Parsing first makes those failures impossible.
//
// # What it removes, and what that costs
//
// By default it removes comments and re-prints. Comments are the largest
// safely-removable share of most real Go source, and removing them cannot
// change what the code does. It does cost the model whatever intent the author
// wrote down — so if the model's job is to review comments, or the chunks are
// documentation-as-code, do not enable this.
//
// With WithBodyElision, function bodies are replaced, leaving the package
// clause, imports, types and signatures. That is a far larger saving and a far
// larger loss: it answers "what is this package's API" and cannot answer "what
// does this function do". Off by default for exactly that reason.
//
// # Safety
//
// Content that does not parse as Go is refused by CanHandle and falls through
// to the next compressor. Output is re-parsed before being returned, and the
// declaration count is compared against the input; if either check fails the
// original is returned unchanged. A compressor that emitted invalid code, or
// quietly dropped a function, would be worse than one that did nothing —
// because the caller could not tell.
//
// Only Go is supported. Other languages need real parsers; tree-sitter is the
// usual answer and it is cgo, which the stdlib-only core cannot take. Those
// belong in a nested module.
type CodeCompressor struct {
	elideBodies bool
	minLines    int
}

// CodeOption configures a CodeCompressor.
type CodeOption func(*CodeCompressor)

// WithBodyElision replaces function bodies, keeping the package clause,
// imports, type declarations and every signature.
//
// LOSSY in a way comment removal is not: the implementation is gone. Use it
// when the model needs an API surface, never when it needs behaviour.
func WithBodyElision() CodeOption {
	return func(c *CodeCompressor) { c.elideBodies = true }
}

// WithMinLines sets the smallest input the compressor will attempt.
// Defaults to DefaultCodeMinLines.
func WithMinLines(n int) CodeOption {
	return func(c *CodeCompressor) { c.minLines = max(n, 0) }
}

// DefaultCodeMinLines is the shortest input worth parsing. A three-line snippet
// has no comments worth removing, so parsing it is pure overhead.
const DefaultCodeMinLines = 5

// NewCodeCompressor returns a CodeCompressor that removes comments.
//
// NOTE: this replaces the v1 no-op placeholder. The signature is unchanged and
// still takes no required arguments, so existing callers keep compiling — but
// the behaviour is not: a registered CodeCompressor now actually claims Go
// chunks instead of always declining. Callers who registered it to pin
// ordering, as the placeholder's docs suggested, will start seeing it work.
func NewCodeCompressor(opts ...CodeOption) *CodeCompressor {
	c := &CodeCompressor{minLines: DefaultCodeMinLines}
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ Compressor = (*CodeCompressor)(nil)

// CanHandle reports whether content parses as a complete Go file.
//
// Deliberately strict: a whole file, not a fragment. A bare function body is
// ambiguous — it could be Go, Java or C — and guessing wrong means handing the
// next compressor content this one has already rewritten.
func (c *CodeCompressor) CanHandle(content string) bool {
	if strings.Count(content, "\n")+1 < c.minLines {
		return false
	}
	// Every Go file has a package clause. Checking before parsing keeps
	// CanHandle cheap on the overwhelmingly common non-Go case.
	if !strings.Contains(content, "package ") {
		return false
	}
	_, err := parseGo(content)
	return err == nil
}

// Compress removes comments, optionally elides bodies, and re-prints.
//
// Returns the content unchanged — not an error — whenever the result would be
// larger, would not parse, or would lose a declaration. "Compressed" must never
// mean "different".
func (c *CodeCompressor) Compress(content string) (string, error) {
	fset, file, err := parseGoWithFset(content)
	if err != nil {
		return "", fmt.Errorf("compress: not valid Go: %w", err)
	}

	before := declCount(file)
	stripComments(file)
	if c.elideBodies {
		elideBodies(file)
	}

	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.TabIndent, Tabwidth: 1}
	if err := cfg.Fprint(&buf, fset, file); err != nil {
		return "", fmt.Errorf("compress: printing: %w", err)
	}
	out := buf.String()

	// Verify before returning. Each of these is a real failure mode, not a
	// hypothetical: printing an AST with detached comments has edge cases, and
	// silently losing a declaration is the worst outcome available.
	roundTripped, err := parseGo(out)
	if err != nil {
		return content, nil
	}
	if declCount(roundTripped) != before {
		return content, nil
	}
	if len(out) >= len(content) {
		return content, nil // no win; never grow a prompt in the name of shrinking it
	}
	return out, nil
}

// stripComments detaches every comment from the AST. Both the file's comment
// list and each node's Doc/Comment fields must be cleared: go/printer reattaches
// from either.
func stripComments(file *ast.File) {
	file.Comments = nil
	file.Doc = nil
	ast.Inspect(file, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.GenDecl:
			d.Doc = nil
		case *ast.FuncDecl:
			d.Doc = nil
		case *ast.Field:
			d.Doc, d.Comment = nil, nil
		case *ast.ValueSpec:
			d.Doc, d.Comment = nil, nil
		case *ast.TypeSpec:
			d.Doc, d.Comment = nil, nil
		case *ast.ImportSpec:
			d.Doc, d.Comment = nil, nil
		}
		return true
	})
}

// elideBodies replaces each function body with a panic, so the result still
// parses and a reader can see the implementation was removed. An empty body
// would instead imply the function legitimately does nothing.
func elideBodies(file *ast.File) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		fn.Body = &ast.BlockStmt{List: []ast.Stmt{
			&ast.ExprStmt{X: &ast.CallExpr{
				Fun:  ast.NewIdent("panic"),
				Args: []ast.Expr{&ast.BasicLit{Kind: token.STRING, Value: `"body elided"`}},
			}},
		}}
	}
}

// declCount counts declared items — specs inside a GenDecl, plus each func —
// which is what the round-trip check compares to prove nothing was lost.
func declCount(file *ast.File) int {
	n := 0
	for _, d := range file.Decls {
		if gen, ok := d.(*ast.GenDecl); ok {
			n += len(gen.Specs)
			continue
		}
		n++
	}
	return n
}

func parseGo(src string) (*ast.File, error) {
	_, file, err := parseGoWithFset(src)
	return file, err
}

func parseGoWithFset(src string) (*token.FileSet, *ast.File, error) {
	fset := token.NewFileSet()
	// ParseComments puts comments in the AST so they can be removed
	// deliberately; without it go/printer reattaches them from source positions.
	file, err := parser.ParseFile(fset, "chunk.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, err
	}
	return fset, file, nil
}
