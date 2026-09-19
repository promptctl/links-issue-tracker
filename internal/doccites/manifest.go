package doccites

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// Held is one citation that resolved to the symbol its sentence names, at the
// time the manifest was generated.
//
// Keyed on the sentence rather than on a line number. A doc line moves whenever
// anything above it is edited, so a manifest keyed on one would report drift
// every time a chapter gained a paragraph — noise in the exact place this gate
// is asking to be believed.
//
// The span and the file are part of the key, because the sentence is not
// unique. A comma list yields one citation per span sharing one text, so
// `commit_lock.go:92, 357-365` is two entries; keyed on the text alone they are
// one, and a holding span answers for a drifted sibling. Same for a citation a
// chapter repeats. The span is a line range in the cited file, so unlike a doc
// line it does not move when the chapter is edited.
//
// The file is there because 7,830 of the 9,813 citations carry no path of their
// own — every shape but the qualified one: two chapters can both cite
// `:125-128` for a String they resolved to different files, and the file is the
// only thing that tells those two entries apart.
type Held struct {
	Doc    string
	Text   string
	File   string
	Span   Span
	Symbol string
}

func (h Held) String() string {
	return fmt.Sprintf("%s %s (%s:%s) → %s", h.Doc, h.Text, h.File, h.Span, h.Symbol)
}

// Holding reduces a survey to the citations that resolved, deduplicated and
// ordered so that a regenerated manifest differs only where the corpus did.
func Holding(findings []Finding) []Held {
	seen := map[Held]bool{}
	for _, f := range findings {
		if f.Verdict == Holds {
			seen[Held{Doc: f.Doc, Text: f.Text, File: f.File, Span: f.Span, Symbol: f.Symbol}] = true
		}
	}
	out := make([]Held, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Doc != out[j].Doc {
			return out[i].Doc < out[j].Doc
		}
		if out[i].Text != out[j].Text {
			return out[i].Text < out[j].Text
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Span != out[j].Span {
			if out[i].Span.Start != out[j].Span.Start {
				return out[i].Span.Start < out[j].Span.Start
			}
			return out[i].Span.End < out[j].Span.End
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out
}

// Lapsed names the manifest entries whose citation no longer resolves.
//
// One direction only, and deliberately. Every entry recorded as holding must
// still hold; a chapter is never required to keep any particular citation, so
// deleting a sentence or rewriting a citation to name a different symbol is an
// ordinary edit that regenerating absorbs. What cannot pass is a citation that
// pointed at its subject and stopped — which is what an edit to the cited file
// does, silently, to every citation below the change.
func Lapsed(manifest []Held, findings []Finding) []Held {
	now := map[Held]bool{}
	for _, h := range Holding(findings) {
		now[h] = true
	}
	var out []Held
	for _, h := range manifest {
		if !now[h] {
			out = append(out, h)
		}
	}
	return out
}

// Explain says what a contributor has to do about one lapsed entry, which is
// not always the same thing.
//
// Every verdict is answered, not the three that were easy to phrase. The
// fall-through below is the one message that tells a contributor to regenerate,
// and regenerating is the single action that erases drift instead of repairing
// it. Reaching it by accident — because a cited symbol was renamed and Unbound
// had no case — would hand exactly that instruction to someone whose chapter
// had just stopped being true. [LAW:no-silent-failure]
func Explain(h Held, findings []Finding) string {
	// Two passes, symbol first. A chapter can write one citation twice in two
	// blocks and bind it to a different symbol each time, so Doc/Text/Span alone
	// can match the sibling that never changed — and then explain the wrong one.
	for _, pass := range []bool{true, false} {
		for _, f := range findings {
			if f.Doc != h.Doc || f.Text != h.Text || f.File != h.File || f.Span != h.Span {
				continue
			}
			if pass && f.Symbol != h.Symbol {
				continue
			}
			switch f.Verdict {
			case Holds:
				return fmt.Sprintf("%s:%d: %s now resolves to `%s`, where it recorded `%s` — one of the two is not what the sentence says",
					f.Doc, f.DocLine, f.Text, f.Symbol, h.Symbol)
			case Moved:
				// f.Declared is where f.Symbol is, which on the second pass is
				// not the symbol this entry recorded. Pairing the two printed
				// "`priorityEntry` is now at :21" naming a line where some
				// other identifier lives — the fabricated instruction this
				// function exists to avoid giving.
				if f.Symbol != h.Symbol {
					return fmt.Sprintf("%s:%d: %s no longer names `%s`; it now binds `%s`, declared at %s:%d — restore the symbol or repoint the citation",
						f.Doc, f.DocLine, f.Text, h.Symbol, f.Symbol, f.File, f.Declared)
				}
				return fmt.Sprintf("%s:%d: %s no longer brackets `%s`, which is now at %s:%d — repoint the citation",
					f.Doc, f.DocLine, f.Text, h.Symbol, f.File, f.Declared)
			case OutOfRange:
				return fmt.Sprintf("%s:%d: %s now names lines outside a %d-line %s — repoint the citation",
					f.Doc, f.DocLine, f.Text, f.Lines, f.File)
			case Unresolved:
				return fmt.Sprintf("%s:%d: %s no longer identifies one file in this tree — repoint the citation",
					f.Doc, f.DocLine, f.Text)
			case Unbound:
				return fmt.Sprintf("%s:%d: %s no longer names `%s`, which %s does not declare — restore the symbol or repoint the citation",
					f.Doc, f.DocLine, f.Text, h.Symbol, f.File)
			}
		}
	}
	// The sentence itself is gone, which is an ordinary edit rather than drift.
	return fmt.Sprintf("%s: %s is no longer in the chapter — regenerate with `go run ./tools/doccites -sync`", h.Doc, h.Text)
}

// Render writes the manifest as Go source.
func Render(held []Held) string {
	var b strings.Builder
	b.WriteString("// Code generated by `go run ./tools/doccites -sync`. DO NOT EDIT.\n\n")
	b.WriteString("package doccites\n\n")
	b.WriteString("// Manifest records every citation that resolved to the symbol its sentence\n")
	b.WriteString("// names, as of the last regeneration. TestCitationsStillResolve holds the\n")
	b.WriteString("// corpus to it: an entry that stops resolving is a chapter pointing a reader\n")
	b.WriteString("// at code that is no longer what it describes.\n")
	b.WriteString("var Manifest = []Held{\n")
	for _, h := range held {
		fmt.Fprintf(&b, "\t{Doc: %q, Text: %q, File: %q, Span: Span{Start: %d, End: %d}, Symbol: %q},\n",
			h.Doc, h.Text, h.File, h.Span.Start, h.Span.End, h.Symbol)
	}
	b.WriteString("}\n")
	return b.String()
}

// Regenerate surveys the corpus and returns the manifest source for it.
func Regenerate(root fs.FS) (string, error) {
	findings, err := Survey(root)
	if err != nil {
		return "", err
	}
	return Render(Holding(findings)), nil
}
