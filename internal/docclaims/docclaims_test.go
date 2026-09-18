package docclaims

import (
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// repoRoot is where the gate runs from. This package sits two levels below the
// repository root, and the check is about the real corpus, so it reads the real
// tree rather than a fixture.
const repoRoot = "../.."

// TestDocumentedClaimsStillShip is the gate this package exists to be.
//
// Three merged tickets falsified documented claims while every CI check stayed
// green, because nothing compared the specification's quoted messages against
// the text that ships. This is that comparison.
//
// A failure here is not a broken test: it is a chapter describing a message the
// product no longer has, or a manifest that no longer matches the tree. Each
// line carries the remedy for its own case — and there is exactly one report,
// because when there were two they twice came to tell a contributor opposite
// things about a single entry in a single run.
func TestDocumentedClaimsStillShip(t *testing.T) {
	if len(Manifest) == 0 {
		t.Fatal("manifest is empty — the gate would pass over anything; regenerate with `go run ./tools/docclaims-sync`")
	}
	derived, err := Derive(os.DirFS(repoRoot), Manifest)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	// Reported entry by entry, in both directions. A bare length mismatch names
	// nothing, leaving a reviewer with two totals and no way to tell a new
	// quotation from a drifted one.
	cmp := derived.Compare(Manifest)
	for _, c := range cmp.Added {
		t.Errorf("not in the committed manifest: %s %q — run `go run ./tools/docclaims-sync`", c.Doc, c.Text)
	}
	for _, d := range cmp.Drifted {
		t.Error(d.Explain())
	}
}

// TestADroppedMessageIsReported is the mutation control on the gate.
//
// "Every manifest entry still ships" passes trivially if the comparison can
// never report anything — an empty manifest, or a match that accepts
// everything.
func TestADroppedMessageIsReported(t *testing.T) {
	kept := "no ready work"
	corpus := Corpus{kept: kept}
	manifest := []Claim{
		{Doc: "08-claims-and-identity.md", Text: kept, Src: kept},
		{Doc: "06-issue-commands.md", Text: "on your path", Src: "blocked on %s (unclaimed, on your path)"},
	}
	// The chapter still quotes both; only one of them still ships.
	derived := Derivation{fresh: manifest[:1], quoted: manifest, corpus: corpus}
	drifted := derived.Compare(manifest).Drifted
	if len(drifted) != 1 {
		t.Fatalf("Compare() reported %d drifted claims, want exactly 1 — the gate cannot see a dropped message", len(drifted))
	}
	if drifted[0].Kind != Stopped {
		t.Errorf("Compare() classified the dropped message as kind %d, want Stopped: %s", drifted[0].Kind, drifted[0].Explain())
	}
	if drifted[0].Text != "on your path" {
		t.Errorf("Compare() reported %q, want the dropped claim", drifted[0].Text)
	}
}

// TestTheGateSeesThroughACoincidentalSubstring is the reason a claim records
// the source it was found in. Matching against the union of all shipped text is
// far too weak: hundreds of this corpus's entries have text sitting inside two
// or more distinct sources (measured 2026-09-18). Here the documented message is deleted and an unrelated one still
// contains its words.
func TestTheGateSeesThroughACoincidentalSubstring(t *testing.T) {
	corpus := Corpus{"some other sentence about deleted_at IS NULL here": "some other sentence about deleted_at IS NULL here"}
	manifest := []Claim{{
		Doc:  "03-store-schema.md",
		Text: "deleted_at IS NULL",
		Src:  "SELECT id FROM issues WHERE deleted_at IS NULL",
	}}
	derived := Derivation{quoted: manifest, corpus: corpus}
	missing := derived.Compare(manifest).Drifted
	if len(missing) != 1 {
		t.Fatal("a deleted message was masked by a coincidental substring in an unrelated source")
	}
	// Reported as a moved anchor, naming the source that carries the words now,
	// because from the corpus alone a reworded message and a coincidence are
	// the same observation — and the one instruction that must never be wrong,
	// "Do NOT regenerate", is reserved for the case where nothing carries them.
	if missing[0].Kind != AnchorMoved || missing[0].Now == "" {
		t.Errorf("Compare() reported kind %d with Now=%q, want a moved anchor naming the coincidental source: %s", missing[0].Kind, missing[0].Now, missing[0].Explain())
	}
}

// TestStableKeepsAnchorsWhenAShorterSourceAppears pins the anchoring against
// churn. tightest picks the shortest containing source, so a newly added
// shorter literal would otherwise retarget unrelated entries and fail the
// freshness test on a branch that changed no documented message — wording
// indistinguishable from real drift, which trains blind regeneration.
func TestStableKeepsAnchorsWhenAShorterSourceAppears(t *testing.T) {
	text := "ORDER BY item_rank ASC"
	long := "SELECT id FROM issues ORDER BY item_rank ASC, id ASC"
	prior := []Claim{{Doc: "03-store-schema.md", Text: text, Src: long}}
	corpus := Corpus{long: long, text: text}

	fresh := Matched([]Claim{{Doc: "03-store-schema.md", Text: text}}, corpus)
	if fresh[0].Src != text {
		t.Fatalf("tightest chose %q, want the shorter source — the premise of this test is wrong", fresh[0].Src)
	}
	stable := Stable(fresh, prior, corpus)
	if stable[0].Src != long {
		t.Errorf("Stable() re-anchored to %q; the recorded source still ships and still carries the quotation", stable[0].Src)
	}
}

// TestStableReanchorsWhenTheRecordedSourceGoes is the other half: stability must
// not become stickiness. A recorded source that stops shipping has to give way,
// or a real deletion would be papered over.
func TestStableReanchorsWhenTheRecordedSourceGoes(t *testing.T) {
	text := "ORDER BY item_rank ASC"
	prior := []Claim{{Doc: "03-store-schema.md", Text: text, Src: "a literal that no longer ships " + text}}
	corpus := Corpus{text: text}
	stable := Stable(Matched([]Claim{{Doc: "03-store-schema.md", Text: text}}, corpus), prior, corpus)
	if stable[0].Src != text {
		t.Errorf("Stable() kept %q, which no longer ships", stable[0].Src)
	}
}

// TestShippedTextReadsProductCodeAndItsEmbeddedAssets pins the corpus. A
// message living in a test file or under tools/ must not count, and a message
// that ships only inside an embedded asset must.
//
// The asset half is not hypothetical: this repository is moving user text out
// of Go literals and into embedded files, and `lit quickstart doctor` — quoted
// in two chapters — exists only in internal/templates/defaults/quickstart.md.
func TestShippedTextReadsProductCodeAndItsEmbeddedAssets(t *testing.T) {
	fsys := fstest.MapFS{
		"go.mod":                      {Data: []byte("module example.test/lit\n\nreplace example.test/driver => ./internal/vendor/driver\n")},
		"cmd/lit/main.go":             {Data: []byte("package main\n\nimport (\n\t_ \"example.test/lit/internal/cli\"\n\t_ \"example.test/driver\"\n)\n\nvar A = \"shipped message here\"\n")},
		"internal/cli/real.go":        {Data: []byte("package cli\n\n//go:embed helptext/*\nvar files embed.FS\n")},
		"internal/cli/helptext/a.txt": {Data: []byte("first embedded message\n")},
		"internal/cli/helptext/b.txt": {Data: []byte("second embedded message\n")},
		"internal/cli/real_test.go":   {Data: []byte("package cli\nvar C = \"test only message\"\n")},
		"internal/cli/testdata/x.go":  {Data: []byte("package cli\nvar D = \"testdata only message\"\n")},
		"tools/thing/main.go":         {Data: []byte("package main\nvar E = \"tool only message\"\n")},
		"artifacts/beads/b.go":        {Data: []byte("package beads\nvar F = \"vendored only message\"\n")},
		// A package inside internal/ that nothing links: the shape that twice
		// leaked a foreign tree into the corpus.
		"internal/unlinked/u.go": {Data: []byte("package unlinked\nvar G = \"unlinked message here\"\n")},
		// A locally replaced module IS source this repo ships — but only the
		// part of it something imports.
		"internal/vendor/driver/d.go":            {Data: []byte("package driver\nvar H = \"vendored linked message\"\n")},
		"internal/vendor/driver/example/main.go": {Data: []byte("package main\nvar I = \"vendored example message\"\n")},
		// Two files the go tool leaves out of a build that a naive ".go and
		// not _test.go" rule takes in. Both sit in a package that certainly
		// does link, so nothing but the file-selection rule keeps them out.
		"internal/cli/_scratch.go": {Data: []byte("package cli\nvar J = \"underscored scratch message\"\n")},
		"internal/cli/generate.go": {Data: []byte("// +build ignore\n\npackage main\nvar K = \"legacy ignored message\"\n")},
		// The comma spelling means AND, so this is built by nothing either. A
		// string comparison against "ignore" reads it as product code.
		"internal/cli/gen_linux.go": {Data: []byte("// +build ignore,linux\n\npackage main\nvar L = \"comma ignored message\"\n")},
		// A satisfiable constraint stays in: the text reaching a user is the
		// union over the platforms lit ships on, not whichever one runs this.
		"internal/cli/plat_darwin.go": {Data: []byte("//go:build darwin\n\npackage cli\nvar M = \"platform variant message\"\n")},
		// A NEGATED constraint is the shape that reads backwards when the
		// expression is evaluated once with every tag true. This is
		// internal/cli/detach_posix.go's spelling, and that file is in every
		// macOS and Linux lit; excluding it drops shipped text from the corpus
		// while the windows-only variant, which no lit here contains, stays.
		"internal/cli/plat_posix.go": {Data: []byte("//go:build !windows\n\npackage cli\nvar N = \"posix variant message\"\n")},
	}
	corpus, err := ShippedText(fsys)
	if err != nil {
		t.Fatalf("ShippedText: %v", err)
	}
	for _, want := range []string{
		"shipped message here", "vendored linked message",
		"platform variant message", "posix variant message",
	} {
		if _, ok := corpus[want]; !ok {
			t.Errorf("%q ships and was not collected", want)
		}
	}
	// Both assets, not just the first: an earlier version returned after the
	// first glob match and silently indexed one file per embed directive.
	for _, want := range []string{"internal/cli/helptext/a.txt", "internal/cli/helptext/b.txt"} {
		if _, ok := corpus[want]; !ok {
			t.Errorf("embedded asset %s was not collected", want)
		}
	}
	// "unlinked" and "vendored example" are the regression: both sit under a
	// root the old scope named wholesale, and neither is reachable from a
	// binary. Admitting them is not merely noise — Matched anchors a quotation
	// to the shortest source holding it, so a stray copy in unlinked code
	// becomes the evidence for a chapter's claim and survives deleting the real
	// message.
	for _, absent := range []string{
		"test only message", "testdata only message", "tool only message",
		"vendored only message", "unlinked message here", "vendored example message",
		"underscored scratch message", "legacy ignored message", "comma ignored message",
	} {
		if _, ok := corpus[absent]; ok {
			t.Errorf("%q counted as shipped; nothing links it", absent)
		}
	}
}

// TestOnlyAnUnsatisfiableConstraintExcludesAFile pins the predicate itself,
// because the corpus test can only show the cases its fixture happens to carry.
// Evaluating a constraint under one assignment — every tag but `ignore` true —
// passes the two `ignore` spellings and inverts every negation, which is the
// whole of what four review rounds walked past.
func TestOnlyAnUnsatisfiableConstraintExcludesAFile(t *testing.T) {
	for _, tc := range []struct {
		line    string
		blocked bool
		why     string
	}{
		{"//go:build ignore", true, "the bare marker no build sets"},
		{"//go:build ignore && linux", true, "AND with a real tag is still unsatisfiable"},
		{"// +build ignore,linux", true, "the legacy comma spelling means AND"},
		{"//go:build linux && !linux", true, "unsatisfiable without naming ignore at all"},
		{"//go:build !windows", false, "detach_posix.go: in every macOS and Linux lit"},
		{"//go:build !darwin && !linux", false, "clone_other.go: satisfiable elsewhere"},
		{"//go:build windows", false, "a platform variant, collected with its siblings"},
		{"//go:build linux", false, "likewise"},
		{"//go:build !ignore", false, "every build satisfies this"},
		{"//go:build (linux && !windows) || (windows && !linux)", false,
			"satisfiable only at a point no fixed sample visits"},
	} {
		if got := neverBuilt(tc.line); got != tc.blocked {
			t.Errorf("neverBuilt(%q) = %v, want %v — %s", tc.line, got, tc.blocked, tc.why)
		}
	}
}

// TestSpansInReadsBothQuotingShapes covers the corpus's two backtick shapes and
// its fenced blocks. A single-backtick pattern alone cuts a “…“ span at its
// inner backtick and protects a fragment instead of the message; reading inside
// fences protects shell transcripts that assert nothing about lit.
func TestSpansInReadsBothQuotingShapes(t *testing.T) {
	src := "Both exit 6 with ``no ready work in %s — not a bare `next` `` and `the backlog is not empty` here.\n" +
		"Short: `a b` and one-word: `NoWork` are not claims.\n" +
		"```\ngo test -short ./... and `a fenced span here`\n```\n"
	got, err := spansIn(src)
	if err != nil {
		t.Fatalf("spansIn: %v", err)
	}
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "no ready work in %s") {
		t.Errorf("double-backtick span not read; got %q", joined)
	}
	if !strings.Contains(joined, "the backlog is not empty") {
		t.Errorf("single-backtick span not read; got %q", joined)
	}
	for _, rejected := range []string{"a b", "NoWork", "a fenced span here"} {
		if slices.Contains(got, rejected) {
			t.Errorf("%q was kept as a claim; it is too short, single-word, or inside a fence", rejected)
		}
	}
}

// TestUnclosedFenceIsAnError covers the silent-truncation case. Blanking the
// rest of a chapter would drop every claim below the stray fence, and the
// regeneration would read as an ordinary "entries left the manifest" diff —
// which CONTRIBUTING tells a reviewer means a sentence stopped describing the
// binary.
// TestACollidingHandleIsRefusedRatherThanOverwritten covers the corpus holding
// two kinds of source in one key space: a Go literal keyed by its own text, an
// embedded asset keyed by its path. A literal is collected only when it
// contains a space and an embed pattern may be quoted to contain one, so the
// spaces are not disjoint and the later write would silently win — leaving Src
// naming a handle that no longer identifies one body.
func TestACollidingHandleIsRefusedRatherThanOverwritten(t *testing.T) {
	// The asset's handle is its path from the module root, which is what a
	// colliding literal has to equal.
	const asset = "internal/cli/helptext/with space.txt"
	fsys := fstest.MapFS{
		"go.mod":          {Data: []byte("module example.test/lit\n")},
		"cmd/lit/main.go": {Data: []byte("package main\n\nimport _ \"example.test/lit/internal/cli\"\n\nfunc main() {}\n")},
		// The literal IS the asset's path, which is the collision.
		"internal/cli/cli.go": {Data: []byte("package cli\n\nimport _ \"embed\"\n\n//go:embed \"helptext/with space.txt\"\nvar help string\n\nvar A = \"" + asset + "\"\n")},
		asset:                 {Data: []byte("some help body\n")},
	}
	_, err := ShippedText(fsys)
	if err == nil {
		t.Fatal("a colliding handle was accepted; one source silently replaced the other and Src names neither")
	}
	if !strings.Contains(err.Error(), "with space.txt") {
		t.Errorf("the refusal does not name the colliding handle, so nobody can act on it: %v", err)
	}
}

// TestContradictoryLegacyLinesExcludeTheFile covers the AND across legacy
// lines. Each line here is satisfiable alone, so a per-line test keeps a file
// that no build compiles and lets its literals compete to anchor a chapter.
func TestContradictoryLegacyLinesExcludeTheFile(t *testing.T) {
	src := "// +build linux\n// +build !linux\n\npackage cli\n\nvar A = \"contradictory build message\"\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "a.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !excludedFromEveryBuild(fset, file) {
		t.Error("a file whose legacy constraints contradict each other counted as product code; nothing compiles it")
	}
}

// TestALegacyConstraintNeedsItsBlankLine covers the one placement rule that
// separates the two spellings. `// +build ignore` sitting directly above the
// package clause is package documentation to the go tool, not a constraint:
// the file builds and ships. `//go:build ignore` in the same position is a
// constraint either way.
//
// Measured with `go list -f '{{.GoFiles}}'` rather than read off the
// documentation, across every placement below. Reading the legacy line as a
// constraint here drops a file every binary contains out of the corpus, and
// the messages it carries then report as having stopped shipping — the gate
// calling true prose false, which is the one failure it must never produce.
func TestALegacyConstraintNeedsItsBlankLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src      string
		excluded bool
	}{
		{"legacy with a blank line is a constraint", "// +build ignore\n\npackage cli\n", true},
		{"legacy without one is documentation", "// +build ignore\npackage cli\n", false},
		{"legacy separated by another comment then a blank", "// +build ignore\n// and a note\n\npackage cli\n", true},
		{"legacy after another comment, blank before package", "// a note\n// +build ignore\n\npackage cli\n", true},
		{"legacy blank-separated from a doc comment", "// +build ignore\n\n// Package cli does things.\npackage cli\n", true},
		{"go:build needs no blank line", "//go:build ignore\npackage cli\n", true},
		{"go:build with a comment between and no blank", "//go:build ignore\n// and a note\npackage cli\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "a.go", tc.src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := excludedFromEveryBuild(fset, file); got != tc.excluded {
				t.Errorf("excludedFromEveryBuild = %v, want %v — the go tool disagrees, so the corpus holds the wrong set of files", got, tc.excluded)
			}
		})
	}
}

// TestAnInlineCodeSpanIsNotAFence pins the CommonMark rule that a backtick
// fence's info string may not contain a backtick. Without it a prose line
// opening with an inline code span opens a fence nothing closes, and every
// claim below it leaves the manifest looking like an ordinary deletion.
func TestAnInlineCodeSpanIsNotAFence(t *testing.T) {
	doc := "intro\n" +
		"```lit next``` prints the pick\n" +
		"and `a quoted message here` follows\n"
	spans, err := spansIn(doc)
	if err != nil {
		t.Fatalf("a line opening with an inline code span was read as a fence: %v", err)
	}
	if !slices.Contains(spans, "a quoted message here") {
		t.Errorf("the span below the inline code span was swallowed as fenced: %q", spans)
	}
}

func TestUnclosedFenceIsAnError(t *testing.T) {
	if _, err := spansIn("intro\n```\nnever closed\n"); err == nil {
		t.Fatal("an unclosed fence was accepted; every claim below it would vanish silently")
	}
}

// TestEmbedPatternsSurviveQuotingAndSpaces covers the directive syntax rather
// than the common case. A quoted pattern holding a space, split on whitespace,
// becomes two patterns that match nothing — and a pattern matching nothing is
// silent, so the asset vanishes from the corpus and every chapter quoting it is
// reported as drifted prose.
func TestEmbedPatternsSurviveQuotingAndSpaces(t *testing.T) {
	got := embedPatterns("plain.txt \"with space.txt\" `raw quoted.txt` all:tree")
	want := []string{"plain.txt", "with space.txt", "raw quoted.txt", "all:tree"}
	if !slices.Equal(got, want) {
		t.Errorf("embedPatterns() = %q, want %q", got, want)
	}
}

// TestDirectoryEmbedOmitsUnderscoredFilesButGlobDoesNot pins the corpus to the
// compiler's rule, which is not the one a reader expects: `//go:embed sub`
// leaves out sub/_x.txt, while `//go:embed d/*` embeds d/_x.txt. Measured
// against the go tool, not read off the documentation. Getting it wrong in
// either direction puts unreachable text in the corpus, or drops text that
// ships.
func TestDirectoryEmbedOmitsUnderscoredFilesButGlobDoesNot(t *testing.T) {
	fsys := fstest.MapFS{
		"go.mod":             {Data: []byte("module example.test/lit\n")},
		"cmd/lit/main.go":    {Data: []byte("package main\n\n//go:embed sub\n//go:embed d/*\nvar f embed.FS\n")},
		"cmd/lit/sub/a.txt":  {Data: []byte("subdir kept message\n")},
		"cmd/lit/sub/_x.txt": {Data: []byte("subdir underscored message\n")},
		"cmd/lit/d/a.txt":    {Data: []byte("glob kept message\n")},
		"cmd/lit/d/_x.txt":   {Data: []byte("glob underscored message\n")},
	}
	corpus, err := ShippedText(fsys)
	if err != nil {
		t.Fatalf("ShippedText: %v", err)
	}
	for _, want := range []string{"cmd/lit/sub/a.txt", "cmd/lit/d/a.txt", "cmd/lit/d/_x.txt"} {
		if _, ok := corpus[want]; !ok {
			t.Errorf("%s is embedded and was not collected", want)
		}
	}
	if _, ok := corpus["cmd/lit/sub/_x.txt"]; ok {
		t.Error("cmd/lit/sub/_x.txt counted as shipped; a directory pattern omits underscored names")
	}
}

// TestNestedFenceDoesNotInvertTheRegion is the fence-parity regression. A single
// boolean toggled by either marker lets a ~~~ inside a ``` block close it, so
// the next ``` reopens a fence over ordinary prose. The quiet failure is real
// claims below the nesting dropped from the manifest, which regenerates as an
// ordinary "entries left" diff — the very signal CONTRIBUTING tells a reviewer
// means a sentence stopped describing the binary.
func TestNestedFenceDoesNotInvertTheRegion(t *testing.T) {
	src := "intro `a real claim here`\n```\n~~~\n```\nafter `another real claim here`\n"
	spans, err := spansIn(src)
	if err != nil {
		t.Fatalf("spansIn: %v", err)
	}
	want := []string{"a real claim here", "another real claim here"}
	if !slices.Equal(spans, want) {
		t.Errorf("spansIn() = %q, want %q — the fenced region swallowed live prose", spans, want)
	}
}

// TestClosingFenceMustMatchItsOpener covers the other half of the rule: a longer
// run opens a fence that a shorter one cannot close, and a marker carrying an
// info string is an opener, never a closer.
func TestClosingFenceMustMatchItsOpener(t *testing.T) {
	if _, err := spansIn("````\n```\nstill inside\n"); err == nil {
		t.Error("a ``` did not close a ```` fence, but no unclosed-fence error was reported")
	}
	spans, err := spansIn("````\nfenced\n````\nafter `a real claim here`\n")
	if err != nil {
		t.Fatalf("spansIn: %v", err)
	}
	if !slices.Equal(spans, []string{"a real claim here"}) {
		t.Errorf("spansIn() = %q, want the claim after the closed fence", spans)
	}
}

// TestTheThreeCasesAreToldApart is the regression for a report that named the
// wrong remedy on the ordinary edit.
//
// A committed entry leaves a fresh derivation three ways, and the classifier
// has twice been too narrow. First it asked one question — does the recorded
// source still carry the words — which puts a literal reworded around a
// quotation in the same bucket as a deleted message, under the loudest
// instruction in the design: "Do NOT regenerate". Then, asking only the corpus,
// it put the *prescribed workflow* there too: delete a message and the sentence
// quoting it together, as CONTRIBUTING asks, and the report told the
// contributor not to regenerate a sentence they had just removed.
//
// Both facts are needed. Whether the chapter still quotes the words comes from
// the documents; whether anything still ships them comes from the corpus. The
// case where a message is deleted while an unrelated string keeps its words
// alive is why "still somewhere in the tree" cannot decide it either: that is
// reported as a moved anchor with the new source named, for a reader to judge,
// and never as a regeneration to wave through.
func TestTheThreeCasesAreToldApart(t *testing.T) {
	const (
		quoted  = "lit quickstart doctor"
		was     = "deeper guidance: lit quickstart doctor\n"
		now     = "further guidance: lit quickstart doctor\n"
		chapter = "06-issue-commands.md"
	)
	entry := Claim{Doc: chapter, Text: quoted, Src: was}
	stillQuoted := []Claim{{Doc: chapter, Text: quoted}}

	for _, tc := range []struct {
		name         string
		derived      Derivation
		want         DriftKind
		wantNow      string
		wantQuotedBy string
	}{{
		name: "the literal was reworded around the quotation",
		derived: Derivation{
			fresh:  []Claim{{Doc: chapter, Text: quoted, Src: now}},
			quoted: stillQuoted,
			corpus: Corpus{now: now},
		},
		want:    AnchorMoved,
		wantNow: now,
	}, {
		name:    "the chapter stopped quoting a message that still ships",
		derived: Derivation{corpus: Corpus{was: was}},
		want:    QuoteDropped,
	}, {
		name:         "the message stopped shipping and the chapter still quotes it",
		derived:      Derivation{quoted: stillQuoted, corpus: Corpus{"an unrelated message": "an unrelated message"}},
		want:         Stopped,
		wantQuotedBy: chapter,
	}, {
		name:    "the message and the sentence quoting it were deleted together",
		derived: Derivation{corpus: Corpus{"an unrelated message": "an unrelated message"}},
		want:    QuoteDropped,
	}, {
		// The recorded (doc, text) key is absent because the sentence is in a
		// different file now, not because anyone stopped asserting it. Asking
		// only about this chapter answers "the prose changed; regenerate", and
		// regenerating leaves 07 describing a message the binary lost.
		name: "the sentence moved to another chapter in the change that deleted the message",
		derived: Derivation{
			quoted: []Claim{{Doc: "07-ops-commands-and-sync-engine.md", Text: quoted}},
			corpus: Corpus{"an unrelated message": "an unrelated message"},
		},
		want: Stopped,
		// The chapter it moved TO, not the entry's own chapter, which is the
		// one that stopped quoting it. Naming Claim.Doc here would send a
		// contributor to the file that is already correct.
		wantQuotedBy: "07-ops-commands-and-sync-engine.md",
	}, {
		// The guard on over-correcting: keying the question on the text alone
		// reports this as a re-anchor, naming a source that never moved. One
		// chapter of several dropping a quotation is an ordinary edit and the
		// entry for THAT chapter should simply go.
		name: "one chapter stopped quoting a message another still quotes, and it still ships",
		derived: Derivation{
			fresh:  []Claim{{Doc: "07-ops-commands-and-sync-engine.md", Text: quoted, Src: was}},
			quoted: []Claim{{Doc: "07-ops-commands-and-sync-engine.md", Text: quoted}},
			corpus: Corpus{was: was},
		},
		want: QuoteDropped,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.derived.Compare([]Claim{entry}).Drifted
			if len(got) != 1 {
				t.Fatalf("Compare() reported %d drifted entries, want exactly 1", len(got))
			}
			if got[0].Kind != tc.want {
				t.Errorf("classified as kind %d, want %d — it reports: %s", got[0].Kind, tc.want, got[0].Explain())
			}
			if got[0].Now != tc.wantNow {
				t.Errorf("named %q as the source carrying it now, want %q", got[0].Now, tc.wantNow)
			}
			if joined := strings.Join(got[0].QuotedBy, ", "); joined != tc.wantQuotedBy {
				t.Errorf("named %q as still quoting it, want %q — it reports: %s", joined, tc.wantQuotedBy, got[0].Explain())
			}
			// The rendered sentence, not only the field behind it. Pinning the
			// field alone let a mutation that printed Claim.Doc in the warning
			// survive: the entry is recorded against the chapter that STOPPED
			// quoting the message, so that sentence sends a contributor to the
			// one file with nothing wrong in it.
			if tc.want == Stopped && !strings.Contains(got[0].Explain(), tc.wantQuotedBy) {
				t.Errorf("Explain() does not name %q as still quoting it: %s", tc.wantQuotedBy, got[0].Explain())
			}
			// The instruction, not the label: only a message that genuinely
			// stopped shipping, and that a chapter still quotes, may carry the
			// one warning that tells a contributor their regeneration would
			// erase evidence.
			warned := strings.Contains(got[0].Explain(), "Do NOT regenerate")
			if warned != (tc.want == Stopped) {
				t.Errorf("Explain() warns against regenerating = %v, want %v — it reports: %s", warned, tc.want == Stopped, got[0].Explain())
			}
		})
	}
}

// TestTheGateIsNotItsOwnEvidence is the one failure this package has already
// had, pinned so it cannot return quietly.
//
// manifest_gen.go holds every documented quotation as a Go string literal, so
// if this package were ever inside the corpus it collects, each entry would be
// satisfied by its own recorded copy: `tightest` would anchor every claim to
// the literal that *is* its text, deleting the real shipped message would change
// nothing, and TestDocumentedClaimsStillShip would stay green over a gate that
// checks nothing.
// That is not hypothetical — an earlier blacklist admitted this package and the
// gate passed over a deliberately mutated message.
//
// Today the exclusion is emergent: nothing under cmd/ imports internal/docclaims,
// so reachability leaves it out. Emergent is not enforced. One import added for
// an unrelated reason — a `lit doctor` subcommand that reports manifest health,
// say — would disable the gate with no failing test anywhere.
func TestTheGateIsNotItsOwnEvidence(t *testing.T) {
	dirs, err := shippedPackages(os.DirFS(repoRoot))
	if err != nil {
		t.Fatalf("shippedPackages: %v", err)
	}
	// Both registries of verbatim document text, not just this one.
	// internal/docsclaims stores quotations from design-docs as Go literals and
	// has already produced a false anchor here — `git remote -v` was held up by
	// its copy of a sentence from docs/architecture.md while the real message
	// could have been deleted freely. It is unimported today for the same
	// incidental reason this package is.
	for _, dir := range []string{"internal/docclaims", "internal/docsclaims"} {
		if slices.Contains(dirs, dir) {
			t.Fatalf("%s is now linked into a binary under cmd/, so its verbatim copies of documented text are inside the corpus this gate checks against: entries anchor to the copy rather than to the shipped message, and deleting the real message changes nothing. Move that text out of the walked import graph before linking the package in.", dir)
		}
	}
}

// TestAnEmbedPatternKeepsItsEscapes covers an operand the compiler accepts and
// this parser used to split into fragments. Cutting at the first inner quote
// yields patterns matching no file, and an unmatched pattern is a hard error —
// so the gate would fail the build over legal source.
func TestAnEmbedPatternKeepsItsEscapes(t *testing.T) {
	got := embedPatterns(`"say \"hi\".txt" plain.txt`)
	want := []string{`say "hi".txt`, "plain.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("embedPatterns() = %q, want %q", got, want)
	}
}

// TestACommentedReplaceIsNotADirective pins go.mod's syntax to the parser the
// go command uses. Reading any line holding an arrow as a replacement takes a
// commented-out one for a live one, and the source list it builds decides which
// packages are searched for shipped text.
func TestACommentedReplaceIsNotADirective(t *testing.T) {
	fsys := fstest.MapFS{"go.mod": {Data: []byte(
		"module example.test/lit\n\n// replace example.test/x => ./commented\n\nreplace example.test/y => ./live\n",
	)}}
	got, err := localSources(fsys)
	if err != nil {
		t.Fatalf("localSources: %v", err)
	}
	for _, s := range got {
		if s.dir == "commented" {
			t.Errorf("a commented-out replace was read as a directive: %+v", s)
		}
	}
	if _, ok := got.dir("example.test/y"); !ok {
		t.Error("the live replace was not read")
	}
}

// TestAnIgnoredMainIsNotAnEntryPoint covers the guard that the gap disarmed. A
// `main` no build compiles is not a binary, and counting one as an entry point
// is worse than missing it: the walk starts from a package whose files are all
// skipped, the corpus comes back empty, and the "no main package" error — whose
// whole purpose is to say the walk is broken rather than the specification
// false — never fires.
func TestAnIgnoredMainIsNotAnEntryPoint(t *testing.T) {
	fsys := fstest.MapFS{
		"go.mod":          {Data: []byte("module example.test/lit\n")},
		"cmd/gen/main.go": {Data: []byte("//go:build ignore\n\npackage main\n\nvar A = \"generator only message\"\n")},
	}
	if _, err := entryPackages(fsys); err == nil {
		t.Fatal("a build-ignored main counted as an entry point; the empty-corpus guard cannot fire")
	}
}

// TestAMovedAnchorIsReportedOnce is the double-report regression. One quotation
// whose literal was reworded leaves the manifest and re-enters the derivation
// under a new source, so it appears in both halves of the comparison — once as
// drift and once as a new quotation, with remedies that read as opposites.
func TestAMovedAnchorIsReportedOnce(t *testing.T) {
	const (
		quoted  = "lit quickstart doctor"
		was     = "deeper guidance: lit quickstart doctor\n"
		now     = "further guidance: lit quickstart doctor\n"
		chapter = "06-issue-commands.md"
	)
	derived := Derivation{
		fresh:  []Claim{{Doc: chapter, Text: quoted, Src: now}},
		quoted: []Claim{{Doc: chapter, Text: quoted}},
		corpus: Corpus{now: now},
	}
	cmp := derived.Compare([]Claim{{Doc: chapter, Text: quoted, Src: was}})
	if len(cmp.Drifted) != 1 || cmp.Drifted[0].Kind != AnchorMoved {
		t.Fatalf("Compare() reported %d drifted entries, want one moved anchor", len(cmp.Drifted))
	}
	if len(cmp.Added) != 0 {
		t.Errorf("the same quotation is also reported as new: %+v — one quotation, two findings, opposite remedies", cmp.Added)
	}
}

// TestAGoModWithoutAModulePathIsAnError covers a guard that tested the wrong
// condition. modfile accepts a go.mod with no `module` line, so one carrying
// any local replace left the source list non-empty and the guard silent —
// after which every import of this repository's own packages fails to resolve,
// the walk yields only the cmd/ entry directories, and every entry reports
// as drifted prose. "The specification is false" is the one thing a broken walk
// must never say.
func TestAGoModWithoutAModulePathIsAnError(t *testing.T) {
	fsys := fstest.MapFS{"go.mod": {Data: []byte("go 1.25\n\nreplace example.test/y => ./live\n")}}
	if _, err := localSources(fsys); err == nil {
		t.Fatal("a go.mod with no module path was accepted; every import would silently fail to resolve")
	}
}
