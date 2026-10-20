package docclaims

import (
	"bytes"
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
			body, start := spanBody(n, source)
			if cutShort(n, body, source) {
				return ast.WalkStop, fmt.Errorf("line %d: the code span %q is cut short by a backtick inside it; quote the whole message in a longer backtick run (``…``)", lineAt(start), body)
			}
			out = append(out, Span{Text: strings.TrimSpace(body), Line: lineAt(start)})
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			if bytes.IndexByte(n.Segment.Value(source), '`') >= 0 {
				return ast.WalkStop, fmt.Errorf("line %d: a backtick renders as text in %q; a span that quotes backticks needs a longer backtick run around it (``…``), a backslash does not escape inside a span, and in a table a pipe inside a span is written \\|", lineAt(n.Segment.Start), n.Segment.Value(source))
			}
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// spanBody is a span's text as rendered, a line ending inside it read as a
// space, and the offset of its first character.
func spanBody(n *ast.CodeSpan, source []byte) (string, int) {
	var body strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		body.Write(c.(*ast.Text).Segment.Value(source))
	}
	return strings.ReplaceAll(body.String(), "\n", " "), n.FirstChild().(*ast.Text).Segment.Start
}

// cutShort reports a span closed early by a backtick inside the quotation.
// `run `x` now` renders as the span "run ", the text "x" and the span " now":
// a space at the span's edge sits against a word outside it, which nobody
// writes on purpose. A per-paragraph backtick count cannot see this; that line
// holds four backticks, an even number.
func cutShort(n *ast.CodeSpan, body string, source []byte) bool {
	if prev, ok := n.PreviousSibling().(*ast.Text); ok && !prev.SoftLineBreak() && !prev.HardLineBreak() {
		r, _ := utf8.DecodeLastRune(prev.Segment.Value(source))
		if isWord(r) && strings.HasPrefix(body, " ") {
			return true
		}
	}
	if next, ok := n.NextSibling().(*ast.Text); ok {
		r, _ := utf8.DecodeRune(next.Segment.Value(source))
		if isWord(r) && strings.HasSuffix(body, " ") {
			return true
		}
	}
	return false
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

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
