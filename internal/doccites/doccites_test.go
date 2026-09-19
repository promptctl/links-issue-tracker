package doccites

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

// repoRoot is where the gate runs from. This package sits two levels below the
// repository root, and the check is about the real corpus, so it reads the real
// tree rather than a fixture.
const repoRoot = "../.."

// TestCitationsStillResolve is the gate this package exists to be.
//
// The specification points at source by line number and nothing kept those
// numbers true: measured when this gate was written, 36.8% of the citations
// that can be checked at all pointed somewhere other than the thing their
// sentence names, and all eleven CI checks were green over every one of them.
//
// The gate is one-directional and baselined, because a corpus that is already
// wrong more often than a third of the time cannot be held to "every citation
// resolves" — that fails on the first run and is switched off. It is held to
// "every citation that resolved still resolves", which is the bleeding rather
// than the wound.
//
// A failure here is not a broken test: it is a chapter sending a reader to code
// that is no longer what it describes.
func TestCitationsStillResolve(t *testing.T) {
	if len(Manifest) == 0 {
		t.Fatal("manifest is empty — the gate would pass over anything; regenerate with `go run ./tools/doccites -sync`")
	}
	findings, err := Survey(os.DirFS(repoRoot))
	if err != nil {
		t.Fatalf("Survey: %v", err)
	}
	for _, h := range Lapsed(Manifest, findings) {
		t.Error(Explain(h, findings))
	}
}

// TestTheGateFiresWhenACitedDeclarationMoves is the mutation control.
//
// "Every recorded citation still resolves" passes trivially if the comparison
// can never report anything, and a gate that cannot fail is indistinguishable
// from one that passes. This moves a declaration out from under its citation
// and requires the report.
func TestTheGateFiresWhenACitedDeclarationMoves(t *testing.T) {
	before := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Widget` is the unit (`internal/a/a.go:1-3`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\ntype Widget struct{}\n")},
	}
	findings, err := Survey(before)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Holding(findings)
	if len(manifest) != 1 {
		t.Fatalf("fixture should yield one holding citation, got %d", len(manifest))
	}

	after := fstest.MapFS{
		"doc-v1-total/x.md": before["doc-v1-total/x.md"],
		// Two lines of padding push Widget out of the cited 1-3.
		"internal/a/a.go": &fstest.MapFile{Data: []byte("package a\n\n// padding\n// padding\ntype Widget struct{}\n")},
	}
	moved, err := Survey(after)
	if err != nil {
		t.Fatal(err)
	}
	lapsed := Lapsed(manifest, moved)
	if len(lapsed) != 1 {
		t.Fatalf("a declaration moved out of its cited span and the gate said nothing: %+v", lapsed)
	}
	if got := Explain(lapsed[0], moved); !strings.Contains(got, "no longer brackets") || !strings.Contains(got, "Widget") {
		t.Errorf("the report does not say what happened: %q", got)
	}
}

// TestEveryCitationShapeIsRead is the property the corpus most needs and the
// one every previous instrument got wrong.
//
// Four spellings occur, three of them abbreviations, and a reader anchored on
// "what follows `file.go:`" sees only the first — it skips bare continuations
// and resolves bare basenames against whatever file it guesses. Four separate
// sweeps made that mistake, the last by an author who had it written down.
func TestEveryCitationShapeIsRead(t *testing.T) {
	doc := "`Alpha` opens it (`internal/a/a.go:3`), `Beta` follows (`:4`), " +
		"`Gamma` and `Delta` sit together (`a.go:5,6`).\n"
	cites := Parse("x.md", doc)
	if len(cites) != 4 {
		t.Fatalf("want 4 citations (named, bare continuation, and both halves of a comma tail), got %d: %+v", len(cites), cites)
	}
	for _, c := range cites {
		if c.Named != "internal/a/a.go" {
			t.Errorf("%s resolved to %q, want internal/a/a.go", c.Text, c.Named)
		}
	}
	if cites[3].Span != (Span{Start: 6, End: 6}) {
		t.Errorf("the second half of the comma tail was dropped: %+v", cites[3].Span)
	}
}

// TestABareBasenameIsNotGuessedAt records the refusal that keeps a verdict
// honest. Three packages in this tree hold a sync.go; a citation reading
// `sync.go:20` under a chapter that never says which one names none of them,
// and choosing one would report a confident verdict about a file the sentence
// never mentioned.
func TestABareBasenameIsNotGuessedAt(t *testing.T) {
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Thing` lives here (`dup.go:1`).\n")},
		"internal/a/dup.go": &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
		"internal/b/dup.go": &fstest.MapFile{Data: []byte("package b\n\ntype Thing struct{}\n")},
	}
	findings, err := Survey(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Verdict != Unresolved {
		t.Fatalf("an ambiguous basename should resolve to nothing, got %+v", findings)
	}
}

// TestTheCorpusQualifiesWhatAChapterAbbreviates covers the case that makes a
// corpus-wide glossary necessary: chapter 04 cites `store.go:1742-1745` and
// never names internal/store/store.go anywhere in the document, because a
// reader takes it from the chapter's subject.
func TestTheCorpusQualifiesWhatAChapterAbbreviates(t *testing.T) {
	g := BuildGlossary(map[string]string{
		"a.md": "the store is `internal/store/store.go`, described at length",
		"b.md": "`Ranked` is computed there (`store.go:2`)",
	})
	cites := g.Apply(Parse("b.md", "`Ranked` is computed there (`store.go:2`)"))
	if len(cites) != 1 || cites[0].Named != "internal/store/store.go" {
		t.Fatalf("the corpus qualifies store.go; this chapter's citation should inherit it: %+v", cites)
	}
}

// TestACitedDocCommentResolves guards a blind spot that was recorded on this
// ticket before the gate existed: a chapter citing a claim cites the sentence
// that states it, which is the doc comment above the declaration rather than
// the declaration line.
func TestACitedDocCommentResolves(t *testing.T) {
	// The cited lines are the comment alone, and they do not contain the word
	// "Midpoint" — otherwise the use-site check would carry this on its own and
	// the doc comment's membership in the declaration would go untested.
	v := verdictOf(t, fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Midpoint` treats empty bounds as the whole keyspace (`internal/a/a.go:3-4`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\n// An empty a means \"before everything\" and an empty b \"after everything\";\n// both empty is the whole keyspace.\nfunc Midpoint(a, b string) string { return \"\" }\n")},
	})
	if v != Holds {
		t.Errorf("a citation of the doc comment that carries the claim should resolve, got %s", v)
	}
}

// TestAUseSiteResolves covers the other legitimate convention. exit.go's
// dispatch arms are cited as the three lines that return a code, which contain
// no declaration at all and are exactly what their sentence means.
func TestAUseSiteResolves(t *testing.T) {
	v := verdictOf(t, fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("a template fault maps to `ExitValidation` (`internal/a/a.go:5-7`).\n")},
		"internal/a/a.go": &fstest.MapFile{Data: []byte(
			"package a\n\nconst ExitValidation = 3\n\nfunc code(bad bool) int {\n\tif bad {\n\t\treturn ExitValidation\n\t}\n\treturn 0\n}\n")},
	})
	if v != Holds {
		t.Errorf("a citation of the lines that use the symbol should resolve, got %s", v)
	}
}

// TestASpanPastTheEndOfTheFileIsReported is the one check that reaches the
// citations no symbol binds to, which is over half the corpus.
func TestASpanPastTheEndOfTheFileIsReported(t *testing.T) {
	v := verdictOf(t, fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("see the tail (`internal/a/a.go:400-410`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n")},
	})
	if v != PastEOF {
		t.Errorf("a span past the end of the file should be reported, got %s", v)
	}
}

// TestACitationInAnotherBulletDoesNotBind keeps the symbol search inside the
// citation's own block. An inventory's list of field changes bound `:233-239`
// to a function named three items further up, and reported a verdict about a
// function that item never mentioned.
func TestACitationInAnotherBulletDoesNotBind(t *testing.T) {
	doc := "- `Widget` is planned here (`internal/a/a.go:3`).\n- `labels` set → canonical (`:9`).\n"
	cites := Parse("x.md", doc)
	if len(cites) != 2 {
		t.Fatalf("want 2 citations, got %d", len(cites))
	}
	for _, name := range cites[1].Nearby {
		if name == "Widget" {
			t.Error("a symbol from the previous bullet bound to this one's citation")
		}
	}
}

// verdictOf surveys a one-citation fixture and returns its verdict.
func verdictOf(t *testing.T, fsys fstest.MapFS) Verdict {
	t.Helper()
	findings, err := Survey(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("fixture should yield exactly one citation, got %d: %+v", len(findings), findings)
	}
	return findings[0].Verdict
}
