package compress

import (
	"strings"
	"unicode"
)

// TextCompressor collapses redundant whitespace in prose. It is deliberately
// the most conservative transformation that still saves tokens:
//
//   - runs of intra-line whitespace (spaces, tabs, non-breaking spaces…)
//     collapse to a single space
//   - trailing and leading whitespace is stripped from every line
//   - runs of three or more blank lines collapse to one blank line (paragraph
//     breaks are meaningful; six of them are not)
//   - the whole string is trimmed
//
// It touches whitespace and nothing else: the sequence of non-whitespace
// tokens in the output is byte-for-byte identical to the input's. There is NO
// semantic summarisation, no stop-word removal, no sentence dropping — those
// change meaning and are out of scope for v1 (docs/spec.md §2.4.4).
type TextCompressor struct {
	// KeepBlankLines, when true, preserves single blank lines between
	// paragraphs (the default). When false, all blank lines are removed —
	// cheaper, but harder for a model to read, so it is opt-in.
	KeepBlankLines bool
}

// NewTextCompressor returns a TextCompressor that preserves paragraph breaks.
func NewTextCompressor() *TextCompressor { return &TextCompressor{KeepBlankLines: true} }

// CanHandle claims any content with at least one non-whitespace character.
// TextCompressor is the intended catch-all, so register it LAST.
func (c *TextCompressor) CanHandle(content string) bool {
	return strings.TrimSpace(content) != ""
}

// Compress collapses whitespace. It never returns an error.
func (c *TextCompressor) Compress(content string) (string, error) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	blankRun := 0

	for _, line := range lines {
		collapsed := collapseSpaces(line)
		if collapsed == "" {
			blankRun++
			continue
		}
		if blankRun > 0 && len(out) > 0 && c.KeepBlankLines {
			out = append(out, "")
		}
		blankRun = 0
		out = append(out, collapsed)
	}
	return strings.Join(out, "\n"), nil
}

// collapseSpaces trims a line and reduces internal whitespace runs to one
// space. \r is whitespace to unicode.IsSpace, so CRLF input is normalised too.
func collapseSpaces(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	pendingSpace := false
	for _, r := range line {
		if unicode.IsSpace(r) {
			if b.Len() > 0 {
				pendingSpace = true
			}
			continue
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

var _ Compressor = (*TextCompressor)(nil)
