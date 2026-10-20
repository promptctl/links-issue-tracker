package docclaims

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Span is one inline code span of a document, trimmed, with the 1-based line
// it opens on so a report can send a reader to it.
type Span struct {
	Text string
	Line int
}

// specMarkdown parses a document the way GitHub renders it, GitHub Flavored
// Markdown, so a span here is exactly a span on the rendered page: paired by
// backtick-run length, read across line breaks, never inside a code block.
//
// Its fenced code blocks are opened by fenceRecorder, registered one priority
// ahead of goldmark's own fenced parser so it claims every fence first.
var specMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithBlockParsers(
		util.Prioritized(fenceRecorder{parser.NewFencedCodeBlockParser()}, 699),
	)),
)

// CodeSpans reads every inline code span of one markdown document, in document
// order. It is the one reader of the specification's code spans, so every gate
// over them agrees on what a span is. [LAW:single-enforcer]
//
// It refuses, naming the line, a document that does not render as written: a
// backtick left over as text, a span cut short by a backtick inside it, or a
// fence nothing closes. Each would otherwise reach the gates as a fragment of
// a quotation, or as nothing, and the manifest would regenerate around it
// without complaint. [LAW:no-silent-failure]
func CodeSpans(src string) ([]Span, error) {
	source := []byte(src)
	lineAt := lineIndex(source)
	var out []Span
	err := ast.Walk(specMarkdown.Parser().Parse(text.NewReader(source)), func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.FencedCodeBlock:
			if _, closed := n.Attribute(fenceClosed); !closed {
				opened, _ := n.Attribute(fenceOpenedAt)
				return ast.WalkStop, fmt.Errorf("unclosed code fence opened at line %d", lineAt(opened.(int)))
			}
		case *ast.CodeSpan:
			raw := spanBody(n, source)
			body := lineEnding.Replace(raw)
			open, _ := runs(n, source)
			if cutShort(n, raw, source) {
				return ast.WalkStop, fmt.Errorf("line %d: the code span %q is cut short by a backtick inside it; quote the whole message in a longer backtick run (``…``)", lineAt(open), body)
			}
			out = append(out, Span{Text: strings.TrimSpace(body), Line: lineAt(open)})
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			if strayBacktick(n.Segment, source) {
				return ast.WalkStop, fmt.Errorf("line %d: a backtick renders as text in %q; a span that quotes backticks needs a longer backtick run around it (``…``), a backslash does not escape inside a span, a backtick meant as text in prose is written \\`, and in a table a pipe inside a span is written \\|", lineAt(n.Segment.Start), n.Segment.Value(source))
			}
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// lineEnding reads each CommonMark line ending inside a span as the space it
// renders as.
var lineEnding = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// spanBody is a span's text as written, line endings and all.
func spanBody(n *ast.CodeSpan, source []byte) string {
	var body strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		body.Write(c.(*ast.Text).Segment.Value(source))
	}
	return body.String()
}

// strayBacktick reports a backtick in prose that no backslash escapes: one
// meant to open or close a span that renders as text instead.
func strayBacktick(seg text.Segment, source []byte) bool {
	for p := seg.Start; p < seg.Stop; p++ {
		if source[p] != '`' {
			continue
		}
		q := p
		for q > 0 && source[q-1] == '\\' {
			q--
		}
		if (p-q)%2 == 0 {
			return true
		}
	}
	return false
}

// cutShort reports a span closed early by a backtick inside the quotation.
// `run `x` now` renders as the span "run ", the text "x" and the span " now".
// The tell is a space typed at a span's edge, facing outward: against a word,
// which nobody writes on purpose, or into text that touches both its own
// backtick run and the next span's, which is the quotation the inner backticks
// cut apart, whatever characters it opens and closes with. A line ending at the
// edge is only the paragraph wrapping. A per-paragraph backtick count cannot
// see any of this; `run `x` now` holds four backticks, an even number.
func cutShort(n *ast.CodeSpan, raw string, source []byte) bool {
	open, close := runs(n, source)
	lead, trail := raw[0] == ' ', raw[len(raw)-1] == ' '
	if lead && isWord(lastRune(source[:open])) || trail && isWord(firstRune(source[close:])) {
		return true
	}
	next := nextSpan(n)
	if next == nil {
		return false
	}
	nextRaw := spanBody(next, source)
	nextOpen, _ := runs(next, source)
	gap := source[close:nextOpen]
	return (trail || nextRaw[0] == ' ') && !unicode.IsSpace(firstRune(gap)) && !unicode.IsSpace(lastRune(gap))
}

// runs is the extent of a span in the source, its backtick runs included: the
// offset of its opening run and the offset just past its closing run. Between
// a run and the body sits nothing, or the one space CommonMark strips from
// each end of a span padded on both.
func runs(n *ast.CodeSpan, source []byte) (open, close int) {
	open = n.FirstChild().(*ast.Text).Segment.Start
	close = n.LastChild().(*ast.Text).Segment.Stop
	for open > 0 && source[open-1] != '`' {
		open--
	}
	for open > 0 && source[open-1] == '`' {
		open--
	}
	for close < len(source) && source[close] != '`' {
		close++
	}
	for close < len(source) && source[close] == '`' {
		close++
	}
	return open, close
}

func nextSpan(n ast.Node) *ast.CodeSpan {
	for s := n.NextSibling(); s != nil; s = s.NextSibling() {
		if span, ok := s.(*ast.CodeSpan); ok {
			return span
		}
	}
	return nil
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func firstRune(b []byte) rune { r, _ := utf8.DecodeRune(b); return r }

func lastRune(b []byte) rune { r, _ := utf8.DecodeLastRune(b); return r }

// lineIndex maps a byte offset to its 1-based line. Line starts are indexed
// once: the inventories run to 446KB and thousands of spans, and counting
// newlines per span is quadratic in that.
func lineIndex(source []byte) func(offset int) int {
	starts := []int{0}
	for i, c := range source {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return func(offset int) int { return sort.SearchInts(starts, offset+1) }
}

var (
	fenceOpenedAt = []byte("docclaims-fence-opened-at")
	fenceClosed   = []byte("docclaims-fence-closed")
)

// fenceRecorder is goldmark's fenced code block parser, recording on each block
// the offset it opened at and whether a closing fence ended it. CommonMark runs
// an unclosed fence to the end of its container, so the parse alone cannot tell
// a fence that closed from one that swallowed every claim below it.
type fenceRecorder struct{ parser.BlockParser }

func (f fenceRecorder) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, line := reader.PeekLine()
	node, state := f.BlockParser.Open(parent, reader, pc)
	if node != nil {
		node.SetAttribute(fenceOpenedAt, line.Start)
	}
	return node, state
}

func (f fenceRecorder) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	state := f.BlockParser.Continue(node, reader, pc)
	if state&parser.Close != 0 {
		node.SetAttribute(fenceClosed, true)
	}
	return state
}
