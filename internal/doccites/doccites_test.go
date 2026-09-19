package doccites

import (
	"go/parser"
	"go/token"
	"io/fs"
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
// numbers true: measured when this gate was written, 36.7% of the citations
// that can be checked at all pointed somewhere other than the thing their
// sentence names, and all eleven CI checks were green over every one of them.
//
// The gate is one-directional and baselined, because a corpus that is already
// wrong in more than a third of the citations it can judge cannot be held to
// "every citation resolves" — that fails on the first run and is switched off. It is held to
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
		// Blank lines, not comments. Two comment lines directly above the type
		// are its doc comment, which is part of the declaration for citation
		// purposes, so the span would still legitimately hold and this control
		// would report a gate failure that is not one.
		"internal/a/a.go": &fstest.MapFile{Data: []byte("package a\n\n\n\ntype Widget struct{}\n")},
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
//
// So: a basename the corpus never qualifies resolves to nothing even when exactly
// one file in the tree bears that name — the case the "unique file of that
// name" fallback would answer, and the only case that tests the rule. An
// earlier fixture put two files named dup.go in the tree, so it passed on the
// ambiguity guard instead and left the fallback live underneath it: 1,618
// citations were resolving through the filesystem while this test reported the
// package did not do that.
func TestABareBasenameIsNotGuessedAt(t *testing.T) {
	fsys := fstest.MapFS{
		"doc-v1-total/x.md":  &fstest.MapFile{Data: []byte("`Thing` lives here (`sole.go:3`).\n")},
		"internal/a/sole.go": &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
	}
	if got := verdictOf(t, fsys); got != Unresolved {
		t.Fatalf("an unqualified basename must not be resolved against the tree, got %s", got)
	}
}

// Two files sharing a multi-segment tail leave it unresolved rather than
// picking one, which is the guard the fixture above used to lean on.
func TestAnAmbiguousPathTailIsNotGuessedAt(t *testing.T) {
	fsys := fstest.MapFS{
		"doc-v1-total/x.md":     &fstest.MapFile{Data: []byte("`Thing` lives here (`dup/dup.go:3`).\n")},
		"internal/a/dup/dup.go": &fstest.MapFile{Data: []byte("package dup\n\ntype Thing struct{}\n")},
		"internal/b/dup/dup.go": &fstest.MapFile{Data: []byte("package dup\n\ntype Thing struct{}\n")},
	}
	if got := verdictOf(t, fsys); got != Unresolved {
		t.Fatalf("an ambiguous path tail must resolve to nothing, got %s", got)
	}
}

// A local declaration inside a function body is not something the file
// declares. Indexing one shadows a top-level declaration of the same name
// further down, and the gate then calls a correct citation drift and sends the
// reader into an unrelated function to look for it.
//
// The cited line is inside the real Widget and does not contain the word, so
// only the declaration extent can carry this citation. A span that named the
// symbol would hold through the use-site arm whether the local shadowed it or
// not, and the mutation would survive.
func TestALocalDeclarationDoesNotShadowTheFileScopeOne(t *testing.T) {
	// The cited line sits inside the local's own extent and does not contain
	// its name, and the file declares no Widget at all. Both halves are
	// load-bearing: a line naming the symbol holds through the use-site arm
	// whatever the index says, and once a name can carry several extents a
	// spurious local no longer displaces a real declaration further down — it
	// is added beside it, so the citation this test used to make held either
	// way and the test had stopped pinning anything.
	src := "package a\n" + // 1
		"\n" + // 2
		"func helper() {\n" + // 3
		"\tvar (\n" + // 4
		"\t\tWidget = compute(\n" + // 5
		"\t\t\t1,\n" + // 6
		"\t\t)\n" + // 7
		"\t)\n" + // 8
		"\t_ = Widget\n" + // 9
		"}\n" + // 10
		"\n" + // 11
		"func compute(n int) int { return n }\n" // 12
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Widget` does the work (`internal/a/a.go:6`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte(src)},
	}
	if got := verdictOf(t, fsys); got != Unbound {
		t.Fatalf("the file declares no Widget, so nothing should bind: got %s", got)
	}
}

// The line after a file's final newline is not a line. Counting it accepts a
// citation one past the end, which is the whole of what OutOfRange decides.
func TestTheLineAfterTheFinalNewlineIsNotALine(t *testing.T) {
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Thing` is here (`internal/a/a.go:4`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
	}
	if got := verdictOf(t, fsys); got != OutOfRange {
		t.Fatalf("line 4 of a 3-line file is past its end, got %s", got)
	}
}

// One span of a comma list holding does not answer for its siblings. Keyed on
// the citation text alone the two spans are one manifest entry, and the gate
// passes over a drifted half.
func TestAHoldingSpanDoesNotCoverItsCommaSibling(t *testing.T) {
	doc := "`Thing` is here (`internal/a/a.go:3,7`).\n"
	before := "package a\n\ntype Thing struct{}\n\nfunc use() {\n\t_ = Thing{}\n\t_ = Thing{}\n}\n"
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte(doc)},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte(before)},
	}
	findings, err := Survey(fsys)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Holding(findings)
	if len(manifest) != 2 {
		t.Fatalf("two spans should record two entries, got %d: %v", len(manifest), manifest)
	}
	// Line 7 stops naming Thing; line 3 still declares it.
	after := "package a\n\ntype Thing struct{}\n\nfunc use() {\n\t_ = Thing{}\n\t_ = 0\n}\n"
	fsys["internal/a/a.go"] = &fstest.MapFile{Data: []byte(after)}
	moved, err := Survey(fsys)
	if err != nil {
		t.Fatal(err)
	}
	lapsed := Lapsed(manifest, moved)
	if len(lapsed) != 1 {
		t.Fatalf("the drifted span should lapse on its own, got %d: %v", len(lapsed), lapsed)
	}
	if lapsed[0].Span.Start != 7 {
		t.Fatalf("the lapsed entry should be the :7 span, got %+v", lapsed[0])
	}
}

// A renamed symbol leaves the sentence in place, so the entry lapses with the
// citation still present. Explained as a missing symbol, never as a missing
// sentence: the missing-sentence message tells the contributor to regenerate,
// which would write the drift into the manifest as the new truth.
func TestARenamedSymbolIsNotExplainedAsADeletedSentence(t *testing.T) {
	doc := "`Thing` is here (`internal/a/a.go:3`).\n"
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte(doc)},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
	}
	findings, err := Survey(fsys)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Holding(findings)
	fsys["internal/a/a.go"] = &fstest.MapFile{Data: []byte("package a\n\ntype Gadget struct{}\n")}
	renamed, err := Survey(fsys)
	if err != nil {
		t.Fatal(err)
	}
	lapsed := Lapsed(manifest, renamed)
	if len(lapsed) != 1 {
		t.Fatalf("the renamed symbol should lapse one entry, got %d", len(lapsed))
	}
	msg := Explain(lapsed[0], renamed)
	if strings.Contains(msg, "-sync") {
		t.Fatalf("a renamed symbol must not be explained as a deleted sentence: %s", msg)
	}
	if !strings.Contains(msg, "Thing") {
		t.Fatalf("the message should name the symbol that went missing: %s", msg)
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

// TestAQualifiedNameBindsOnItsLastElement covers the spelling these
// inventories use constantly. The prose writes `model.Attribution` for what the
// source declares as Attribution, and binding on the package qualifier instead
// would resolve nothing — every such citation would fall to Unbound and be
// counted as unjudgeable rather than checked.
func TestAQualifiedNameBindsOnItsLastElement(t *testing.T) {
	v := verdictOf(t, fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`model.Attribution` is opaque (`internal/a/a.go:3`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\ntype Attribution struct{}\n")},
	})
	if v != Holds {
		t.Errorf("a qualified name should bind on Attribution, got %s", v)
	}
}

// TestASpanPastTheEndOfTheFileIsReported is the one check that reaches the
// citations no symbol binds to, which is over half the corpus.
func TestASpanPastTheEndOfTheFileIsReported(t *testing.T) {
	v := verdictOf(t, fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("see the tail (`internal/a/a.go:400-410`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n")},
	})
	if v != OutOfRange {
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

// A malformed span is a finding about one citation, not a crash that stops the
// other 9,812 being reported. `:0` reached the symbol search, which indexes
// lines[Start-1] and panicked the whole run on lines[-1].
func TestAMalformedSpanIsReportedRatherThanFatal(t *testing.T) {
	for _, written := range []string{"internal/a/a.go:0", "internal/a/a.go:0-5", "internal/a/a.go:5-1"} {
		fsys := fstest.MapFS{
			"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Thing` is here (`" + written + "`).\n")},
			"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
		}
		if got := verdictOf(t, fsys); got != OutOfRange {
			t.Errorf("%s should be out-of-range, got %s", written, got)
		}
	}
}

// A file declares one name as many times as it has methods with that name, so
// "the declaration of Error in errors.go" is not a thing that exists. Keeping
// only the first made every citation of the others read as drift.
//
// The cited line is inside the second method and does not contain the word, so
// only the declaration extent can carry it.
func TestACitationOfTheSecondSameNamedMethodHolds(t *testing.T) {
	src := "package a\n" + // 1
		"\n" + // 2
		"type A struct{}\n" + // 3
		"\n" + // 4
		"// Message for A.\n" + // 5
		"func (a A) Error() string {\n" + // 6
		"\treturn \"a\"\n" + // 7
		"}\n" + // 8
		"\n" + // 9
		"type B struct{}\n" + // 10
		"\n" + // 11
		"// Message for B.\n" + // 12
		"func (b B) Error() string {\n" + // 13
		"\treturn \"b\"\n" + // 14
		"}\n" // 15
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`B.Error()` returns it (`internal/a/a.go:14`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte(src)},
	}
	if got := verdictOf(t, fsys); got != Holds {
		t.Fatalf("a citation of the second Error should hold, got %s", got)
	}
}

// "The doc comment is part of the declaration" is stated for every kind of
// declaration, and go/parser attaches a type's or a constant's comment to the
// enclosing GenDecl, not to the spec. Reading the spec alone applied the rule
// to functions and silently to nothing else.
//
// The comment does not contain the symbol, so only the extent rule can carry it.
func TestACitedTypeDocCommentResolves(t *testing.T) {
	src := "package a\n\n// The thing this package is about.\ntype Widget struct{}\n"
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Widget` is described here (`internal/a/a.go:3`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte(src)},
	}
	if got := verdictOf(t, fsys); got != Holds {
		t.Fatalf("a citation of a type's doc comment should hold, got %s", got)
	}
}

// A directory whose own .gitignore excludes everything in it is not part of the
// repository. Indexing one lets a file nobody tracks collide with a real path
// tail and flip a citation to Unresolved — the gate then fails on one machine
// and passes on another, which is the whole thing this package refuses to be.
func TestAWhollyIgnoredDirectoryIsNotIndexed(t *testing.T) {
	fsys := fstest.MapFS{
		"doc-v1-total/x.md":  &fstest.MapFile{Data: []byte("`Thing` lives here (`a/dup.go:3`).\n")},
		"internal/a/dup.go":  &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
		"scratch/.gitignore": &fstest.MapFile{Data: []byte("*\n")},
		"scratch/a/dup.go":   &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
	}
	if got := verdictOf(t, fsys); got != Holds {
		t.Fatalf("an untracked copy must not make the tail ambiguous, got %s", got)
	}
}

// Explain must describe the entry it was handed. One citation written twice in
// two blocks binds two symbols and records two entries; matching on the text
// alone can pick up the sibling that never changed and report it as the problem.
func TestExplainDescribesTheEntryItWasGiven(t *testing.T) {
	cite := Citation{Doc: "d.md", Text: "`a.go:3`", Span: Span{Start: 3, End: 3}}
	steady := Finding{Citation: cite, Verdict: Holds, Symbol: "Alpha", File: "a.go"}
	steady.DocLine = 10
	drifted := Finding{Citation: cite, Verdict: Moved, Symbol: "Beta", Declared: 91, File: "a.go"}
	drifted.DocLine = 20

	h := Held{Doc: "d.md", Text: "`a.go:3`", File: "a.go", Span: Span{Start: 3, End: 3}, Symbol: "Beta"}
	got := Explain(h, []Finding{steady, drifted})
	if !strings.Contains(got, "no longer brackets") || !strings.Contains(got, "Beta") {
		t.Fatalf("the message should describe Beta's drift, got %q", got)
	}
	if strings.Contains(got, "Alpha") {
		t.Fatalf("the message describes the untouched sibling: %q", got)
	}
}

// Nearby is ambiguous by construction, so a citation consistent with any symbol
// its sentence names is consistent with the sentence. Returning on the first
// declared name made the verdict depend on word order: adding a second
// backticked identifier to a sentence — what CONTRIBUTING asks authors to do —
// rebound a holding citation and failed the gate with no code changed.
func TestASecondNamedSymbolDoesNotUnbindAHoldingCitation(t *testing.T) {
	src := "package a\n\ntype Thing struct{}\n\nfunc Helper() {\n}\n"
	docs := []string{
		"`Thing` is the unit (`internal/a/a.go:3`).\n",
		"`Thing` is the unit, built by `Helper` (`internal/a/a.go:3`).\n",
		"`Helper` builds the unit `Thing` (`internal/a/a.go:3`).\n",
	}
	for _, doc := range docs {
		fsys := fstest.MapFS{
			"doc-v1-total/x.md": &fstest.MapFile{Data: []byte(doc)},
			"internal/a/a.go":   &fstest.MapFile{Data: []byte(src)},
		}
		if got := verdictOf(t, fsys); got != Holds {
			t.Errorf("%q should hold, got %s", doc, got)
		}
	}
}

// An interface's methods are declarations of the file that declares the
// interface. The cited line is the method, which does name it, so this pins
// that the method is reachable at all rather than which arm carries it.
func TestAnInterfaceMethodIsADeclaration(t *testing.T) {
	src := "package a\n\ntype Store interface {\n\tApply(x int) error\n}\n"
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Apply` is on the interface (`internal/a/a.go:4`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte(src)},
	}
	if got := verdictOf(t, fsys); got != Holds {
		t.Fatalf("an interface method should bind, got %s", got)
	}
}

// A slash-free .gitignore entry applies at any depth, the way git reads it.
// Anchoring it to the root left this tree's tools/session-analysis/__pycache__
// indexed while the root ignored `__pycache__/`.
func TestASlashFreeIgnoreEntryAppliesAtAnyDepth(t *testing.T) {
	fsys := fstest.MapFS{
		".gitignore":               &fstest.MapFile{Data: []byte("junk/\n")},
		"doc-v1-total/x.md":        &fstest.MapFile{Data: []byte("`Thing` lives here (`a/dup.go:3`).\n")},
		"internal/a/dup.go":        &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
		"tools/deep/junk/a/dup.go": &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
	}
	if got := verdictOf(t, fsys); got != Holds {
		t.Fatalf("a nested ignored directory must not make the tail ambiguous, got %s", got)
	}
}

// An ignored file is as much outside the repository as an ignored directory.
// Reading them anyway line-counted the multi-megabyte binary the root
// .gitignore exists to keep out, on every run.
func TestAnIgnoredFileIsNotIndexed(t *testing.T) {
	fsys := fstest.MapFS{
		".gitignore":        &fstest.MapFile{Data: []byte("/lit\n")},
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Thing` is here (`internal/a/a.go:3`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
		"lit":               &fstest.MapFile{Data: []byte("binary\n")},
	}
	tree, err := Index(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, indexed := tree.lines["lit"]; indexed {
		t.Fatal("an ignored file was read and line-counted")
	}
}

// Explain must not pair the symbol an entry recorded with the line some other
// symbol is declared on. On the fall-back pass those describe different things,
// and the message named a line where the recorded symbol does not appear.
func TestExplainDoesNotPairARecordedSymbolWithAnotherSymbolsLine(t *testing.T) {
	cite := Citation{Doc: "d.md", Text: "`a.go:3`", Span: Span{Start: 3, End: 3}}
	rebound := Finding{Citation: cite, Verdict: Moved, Symbol: "Priorities", Declared: 21, File: "a.go"}
	rebound.DocLine = 7

	h := Held{Doc: "d.md", Text: "`a.go:3`", File: "a.go", Span: Span{Start: 3, End: 3}, Symbol: "priorityEntry"}
	got := Explain(h, []Finding{rebound})
	if strings.Contains(got, "`priorityEntry`, which is now at") {
		t.Fatalf("the message claims priorityEntry moved to another symbol's line: %q", got)
	}
	if !strings.Contains(got, "priorityEntry") || !strings.Contains(got, "Priorities") {
		t.Fatalf("the message should name both the recorded and the bound symbol: %q", got)
	}
}

// One chapter can cite the same span text against two different files, because
// only the qualified shape carries a path — a bare `:3` means whichever file
// the prose last named. Doc, Text, Span and Symbol are then all identical and
// the file is the only discriminator; without it the two collapse to one entry
// and a holding one answers for a drifted one.
//
// Two blocks of one document, not two documents: two documents already differ
// in Doc, so that fixture pins nothing about the file.
func TestOneChapterCitingTwoFilesRecordsTwoEntries(t *testing.T) {
	doc := "In `internal/a/a.go`, `Thing` is the unit (`:3`).\n" +
		"\n" +
		"In `internal/b/b.go`, `Thing` is the unit (`:3`).\n"
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte(doc)},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
		"internal/b/b.go":   &fstest.MapFile{Data: []byte("package b\n\ntype Thing struct{}\n")},
	}
	findings, err := Survey(fsys)
	if err != nil {
		t.Fatal(err)
	}
	held := Holding(findings)
	if len(held) != 2 {
		t.Fatalf("two citations against two files should record two entries, got %d: %v", len(held), held)
	}
	if held[0].File == held[1].File {
		t.Fatalf("both entries resolved to %s, so the fixture does not test the file", held[0].File)
	}
}

// A file rename with no edit to the chapter. The sentence and its citation are
// exactly where they were, so "no longer in the chapter — regenerate" is false,
// and following it deletes the record that the citation now points at nothing.
//
// The mutation this kills is the one the code shipped with: matching a lapsed
// entry only against findings whose File equals the recorded one. An unresolved
// citation has no file at all, so that test can never be satisfied and the
// Unresolved message was unreachable for every corpus.
func TestACitationWhoseFileStoppedResolvingIsNotExplainedAsADeletedSentence(t *testing.T) {
	doc := "`Thing` is here (`internal/a/a.go:3`).\n"
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte(doc)},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
	}
	findings, err := Survey(fsys)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Holding(findings)
	if len(manifest) != 1 {
		t.Fatalf("the fixture should record one entry, got %d", len(manifest))
	}
	delete(fsys, "internal/a/a.go")
	fsys["internal/b/b.go"] = &fstest.MapFile{Data: []byte("package b\n\ntype Thing struct{}\n")}
	renamed, err := Survey(fsys)
	if err != nil {
		t.Fatal(err)
	}
	lapsed := Lapsed(manifest, renamed)
	if len(lapsed) != 1 {
		t.Fatalf("the renamed file should lapse one entry, got %d", len(lapsed))
	}
	msg := Explain(lapsed[0], renamed)
	if strings.Contains(msg, "-sync") {
		t.Fatalf("a citation still in the chapter must not be explained as a deleted sentence: %s", msg)
	}
	if !strings.Contains(msg, "internal/a/a.go") {
		t.Fatalf("the message should name the file the entry recorded: %s", msg)
	}
}

// A citation with no path of its own inherits one from the prose above it, so
// an edit to a sentence the citation is nowhere near can move which file it
// resolves to. The entry lapses — the file is part of the key — and what
// happened is that the path changed, not that anything was deleted.
func TestACitationThatNowResolvesToAnotherFileSaysSo(t *testing.T) {
	cite := Citation{Doc: "d.md", Text: "`:3`", Span: Span{Start: 3, End: 3}}
	moved := Finding{Citation: cite, Verdict: Holds, Symbol: "Thing", File: "internal/b/b.go"}
	moved.DocLine = 12

	h := Held{Doc: "d.md", Text: "`:3`", File: "internal/a/a.go", Span: Span{Start: 3, End: 3}, Symbol: "Thing"}
	got := Explain(h, []Finding{moved})
	if strings.Contains(got, "-sync") {
		t.Fatalf("a citation that moved file is not a deleted sentence: %q", got)
	}
	if !strings.Contains(got, "internal/a/a.go") || !strings.Contains(got, "internal/b/b.go") {
		t.Fatalf("the message should name both the recorded and the current file: %q", got)
	}
}

// Two instances of one citation that a recorded symbol no longer tells apart.
// Nothing here can say which of them the entry was, so the message says so
// rather than describing the first as though it were the one that broke.
func TestExplainSaysWhenTheSameCitationIsWrittenTwice(t *testing.T) {
	cite := Citation{Doc: "d.md", Text: "`a.go:3`", Span: Span{Start: 3, End: 3}}
	first := Finding{Citation: cite, Verdict: Moved, Symbol: "Alpha", Declared: 40, File: "a.go"}
	first.DocLine = 10
	second := Finding{Citation: cite, Verdict: Moved, Symbol: "Beta", Declared: 91, File: "a.go"}
	second.DocLine = 20

	h := Held{Doc: "d.md", Text: "`a.go:3`", File: "a.go", Span: Span{Start: 3, End: 3}, Symbol: "Gamma"}
	got := Explain(h, []Finding{first, second})
	if !strings.Contains(got, "more than once") {
		t.Fatalf("two candidates and no way to tell them apart should be said out loud: %q", got)
	}

	// And the note stays off when the recorded symbol does tell them apart.
	h.Symbol = "Beta"
	if got := Explain(h, []Finding{first, second}); strings.Contains(got, "more than once") {
		t.Fatalf("the recorded symbol identifies one of the two, so there is no ambiguity to report: %q", got)
	}
}

// The same rule as the named function above, reached by the other route. A
// closure assigned at package level hangs off a GenDecl, which the walk
// descends, so a `var` inside its body was indexed as a declaration of the
// file. A citation to an unrelated span then bound on that local and answered
// for the real declaration further down.
func TestALocalInsideAPackageLevelClosureIsNotADeclaration(t *testing.T) {
	// The cited line is inside the local's own extent and does not contain its
	// name. Citing a line that names the symbol holds through the use-site arm
	// whatever the index says, and citing a line outside the local's extent is
	// answered by the real declaration — either fixture passes with the bug in
	// place, which is how the named-function case was first "covered".
	src := "package a\n" + // 1
		"\n" + // 2
		"var Handler = func() {\n" + // 3
		"\tvar (\n" + // 4
		"\t\tWidget = compute(\n" + // 5
		"\t\t\t1,\n" + // 6
		"\t\t)\n" + // 7
		"\t)\n" + // 8
		"\t_ = Widget\n" + // 9
		"}\n" + // 10
		"\n" + // 11
		"func compute(n int) int { return n }\n" // 12
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Widget` does the work (`internal/a/a.go:6`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte(src)},
	}
	if got := verdictOf(t, fsys); got != Unbound {
		t.Fatalf("the file declares no Widget, so nothing should bind: got %s", got)
	}
}

// A path written from the repository root. `/LICENSE-REPORT.md` is how the
// platform chapters quote one, and it reached none of resolve's branches: a
// single segment leaves the suffix walk nothing to trim, and tails are
// registered only from three segments up. Trimming the slash first makes it an
// ordinary repository-relative lookup — not the basename search this refuses,
// because the corpus wrote the whole path.
func TestAPathWrittenFromTheRootResolves(t *testing.T) {
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Thing` is the unit (`/root.go:3`).\n")},
		"root.go":           &fstest.MapFile{Data: []byte("package main\n\ntype Thing struct{}\n")},
	}
	if got := verdictOf(t, fsys); got != Holds {
		t.Fatalf("a citation of a root-level file written from the root should hold, got %s", got)
	}
}

// An absolute path with a machine's checkout baked into it. Eight inventories
// were written that way, and the longest suffix that is a real file is what
// recovers them on any other computer.
func TestAnAbsolutePathResolvesByItsLongestRealSuffix(t *testing.T) {
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{
			Data: []byte("`Thing` is the unit (`/home/someone/checkout/internal/a/a.go:3`).\n")},
		"internal/a/a.go": &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
	}
	if got := verdictOf(t, fsys); got != Holds {
		t.Fatalf("an absolute path should resolve by its longest real suffix, got %s", got)
	}
}

// The generated manifest is the baseline the gate trusts, so what generates it
// has to produce Go that compiles and entries that say what the survey found.
func TestTheGeneratedManifestIsValidGoCarryingWhatTheSurveyFound(t *testing.T) {
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Thing` is the unit (`internal/a/a.go:3`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
	}
	src, err := Regenerate(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "manifest_gen.go", src, 0); err != nil {
		t.Fatalf("the generated manifest does not parse as Go: %v\n%s", err, src)
	}
	for _, want := range []string{`Doc: "doc-v1-total/x.md"`, "Text: \"`internal/a/a.go:3`\"", `File: "internal/a/a.go"`, `Symbol: "Thing"`} {
		if !strings.Contains(src, want) {
			t.Fatalf("the generated manifest is missing %s:\n%s", want, src)
		}
	}
}

// errorFS fails one named read and behaves like its wrapped filesystem
// otherwise, which is how a .gitignore that exists but cannot be read is put in
// front of the indexer.
type errorFS struct {
	fs.FS
	fail string
}

func (e errorFS) Open(name string) (fs.File, error) {
	if name == e.fail {
		return nil, fs.ErrPermission
	}
	return e.FS.Open(name)
}

// A .gitignore that is there and unreadable is not the same answer as one that
// is not there. Read as "no rules here" it indexes a directory the repository
// excludes, changing what the gate measures with nothing said about it.
//
// Asked of ignoredPaths directly, because Index reads every file in the tree
// and would fail on this one a second time by its own route — a test through
// Index passes whether or not the reader distinguishes the two cases, which is
// no test of the reader at all.
func TestAnUnreadableIgnoreFileIsReportedRatherThanTreatedAsAbsent(t *testing.T) {
	base := fstest.MapFS{
		".gitignore": &fstest.MapFile{Data: []byte("vendor\n")},
		"a.go":       &fstest.MapFile{Data: []byte("package a\n")},
	}
	if _, err := ignoredPaths(errorFS{FS: base, fail: ".gitignore"}); err == nil {
		t.Fatal("an unreadable .gitignore should be reported, not read as an empty one")
	}
	if _, err := ignoredPaths(base); err != nil {
		t.Fatalf("a readable .gitignore should not be an error: %v", err)
	}
	// A tree with no .gitignore at all is the ordinary case and not an error.
	if _, err := ignoredPaths(fstest.MapFS{"a.go": &fstest.MapFile{Data: []byte("package a\n")}}); err != nil {
		t.Fatalf("an absent .gitignore is an answer, not a failure: %v", err)
	}
}

// The citation pattern and the path pattern have to admit the same file types.
// A type one reads and the other does not parses as a citation and never as a
// mention, so every abbreviation under it inherits nothing and resolves to
// nothing — with both patterns individually correct and no test failing.
func TestTheTwoPatternsAdmitTheSameFileTypes(t *testing.T) {
	exts := strings.Split(citedExt, "|")
	if len(exts) < 2 {
		t.Fatalf("citedExt does not look like an alternation: %q", citedExt)
	}
	for _, ext := range exts {
		if !citeRe.MatchString("`dir/f." + ext + ":3`") {
			t.Errorf("citeRe does not read a citation of a .%s file", ext)
		}
		if !pathRe.MatchString("`dir/f." + ext + "`") {
			t.Errorf("pathRe does not read a mention of a .%s file", ext)
		}
	}
}
