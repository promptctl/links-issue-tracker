package docclaims

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestCodeSpansReadsWhatGitHubRenders pins the spans a reader of the rendered
// page sees, in document order, including the shapes a regex reader got wrong:
// a span wrapped across lines, a run longer than two, and a span quoting the
// backticks of the message it holds.
func TestCodeSpansReadsWhatGitHubRenders(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want []string
	}{
		{"single", "say `a quoted message` now\n", []string{"a quoted message:1"}},
		{"double quoting inner backticks", "``run `lit next` first`` now\n", []string{"run `lit next` first:1"}},
		{"padded double around a quoted span", "`` `lit next` `` now\n", []string{"`lit next`:1"}},
		{"triple around a double run", "```a `` b``` now\n", []string{"a `` b:1"}},
		{"a lone backslash", "no `/`, `\\`, or `..`\n", []string{"/:1", "\\:1", "..:1"}},
		{"wrapped across lines", "intro\nrun `lit start\n  <id>` then\n", []string{"lit start <id>:2"}},
		{"a deliberate trailing space before a space", "renders `<digest>; ` followed by the command\n", []string{"<digest>;:1"}},
		{"punctuation against a padded edge", "(`  - <column>`) and\n", []string{"- <column>:1"}},
		{"an escaped pipe in a table cell", "| a | b |\n|---|---|\n| `x\\|y` | z |\n", []string{"x|y:3"}},
		{"nothing inside a fence", "```\n`fenced span`\n```\nafter `real one`\n", []string{"real one:4"}},
		{"nothing inside an indented code block", "intro\n\n    `indented span`\n\nafter `real one`\n", []string{"real one:5"}},
		{"a backslash-escaped backtick in prose", "prose with an escaped \\` backtick and `a span`\n", []string{"a span:1"}},
		{"a span opening at a line's end after a word", "word`\n tail` here\n", []string{"tail:1"}},
		{"a span closing at a line's start before a word", "`a\n`b c\n", []string{"a:1"}},
		{"a span whose opening run ends a line", "intro `\nfoo bar\n` end\n\n``\nbaz qux\n`` end\n", []string{"foo bar:1", "baz qux:5"}},
		{"a span wrapped across a CRLF line ending", "a `run\r\nit` b\r\n", []string{"run it:1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spans, err := CodeSpans(tc.doc)
			if err != nil {
				t.Fatalf("CodeSpans refused a document that renders as written: %v", err)
			}
			var got []string
			for _, s := range spans {
				got = append(got, fmt.Sprintf("%s:%d", s.Text, s.Line))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("spans = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCodeSpansRefusesWhatDoesNotRenderAsWritten covers each shape whose
// rendering differs from what its author wrote. Every one used to reach the
// gates as a fragment of a quotation, protected as though it were the message.
// The refusal names the line, so the fix starts where the error points.
func TestCodeSpansRefusesWhatDoesNotRenderAsWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		line string
	}{
		{"an unpaired backtick", "intro\nthe ` stray tick\n", "line 2:"},
		{"a backslash-escaped inner backtick", "x\ny\n`run \\`just install\\` to refresh`\n", "line 3:"},
		{"an inner span closing the outer early", "`run `just install` to refresh`\n", "line 1:"},
		{"a span glued to the word before it", "intro\n\nword` tail` here\n", "line 3:"},
		{"a span glued to the word after it", "`head `word here\n", "line 1:"},
		{"an inner span opening and closing on punctuation", "`see `(a)` now`\n", "line 1:"},
		{"an inner span closing against a comma", "x\n`x `--flag`, then`\n", "line 2:"},
		{"a backtick after an escaped backslash", "a \\\\` stray\n", "line 1:"},
		{"an unescaped pipe splitting a table cell's span", "| a |\n|---|\n| `x|y` |\n", "line 3:"},
		{"an unclosed fence", "intro\n```\nnever closed\n", "line 2"},
		{"a shorter fence cannot close a longer one", "````\n```\nstill inside\n", "line 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spans, err := CodeSpans(tc.doc)
			if err == nil {
				t.Fatalf("accepted a document that does not render as written; read %q", spans)
			}
			if !strings.Contains(err.Error(), tc.line) {
				t.Errorf("error %q does not name %q", err, tc.line)
			}
		})
	}
}
