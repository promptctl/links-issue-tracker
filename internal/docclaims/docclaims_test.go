package docclaims

import (
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
// product no longer has. Fix the prose, then regenerate the manifest with
// `go run ./tools/docclaims-sync`.
func TestDocumentedClaimsStillShip(t *testing.T) {
	corpus, err := ShippedText(os.DirFS(repoRoot))
	if err != nil {
		t.Fatalf("ShippedText: %v", err)
	}
	if len(Manifest) == 0 {
		t.Fatal("manifest is empty — the gate would pass over anything; regenerate with `go run ./tools/docclaims-sync`")
	}
	missing := Missing(Manifest, corpus)
	for _, c := range missing {
		t.Errorf("%s quotes a message that no longer ships:\n  %q\nrecorded against:\n  %q\nEither the prose is now false and should be corrected, or the message moved deliberately and the manifest needs `go run ./tools/docclaims-sync`.", c.Doc, c.Text, c.Src)
	}
	if len(missing) > 0 {
		t.Logf("%d of %d documented quotations have drifted", len(missing), len(Manifest))
	}
}

// TestManifestIsCurrent fails when the committed manifest disagrees with what
// this tree yields, naming every entry on both sides.
//
// Without it nothing compares the two, which is how a manifest derived over a
// working tree carrying a gitignored vendored project was committed: 14 of its
// entries were satisfied only by that project's literals, and the gate passed
// locally while failing in every clean checkout.
func TestManifestIsCurrent(t *testing.T) {
	fresh, err := Derive(os.DirFS(repoRoot), Manifest)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	// Reported entry by entry, in both directions. A bare length mismatch names
	// nothing, leaving a reviewer with two totals and no way to tell a new
	// quotation from a drifted one.
	for _, c := range Diff(fresh, Manifest) {
		t.Errorf("not in the committed manifest: %s %q — run `go run ./tools/docclaims-sync`", c.Doc, c.Text)
	}
	corpus, err := ShippedText(os.DirFS(repoRoot))
	if err != nil {
		t.Fatalf("ShippedText: %v", err)
	}
	// Told apart rather than reported together: for one of these two the
	// remediation is to regenerate, and for the other regenerating is what
	// erases the evidence that a documented message stopped shipping.
	stopped, rephrased := Vanished(Manifest, fresh, corpus)
	for _, c := range rephrased {
		t.Errorf("no longer quoted by %s: %q — the prose changed and the manifest needs `go run ./tools/docclaims-sync`", c.Doc, c.Text)
	}
	for _, c := range stopped {
		t.Errorf("%s quotes a message that no longer ships: %q — fix the code or the chapter. Do NOT regenerate: that drops the entry and leaves the sentence false.", c.Doc, c.Text)
	}
}

// TestMissingReportsADroppedMessage is the mutation control on the gate.
//
// "Every manifest entry still ships" passes trivially if Missing can never
// report anything — an empty manifest, or a match that accepts everything.
func TestMissingReportsADroppedMessage(t *testing.T) {
	kept := "no ready work"
	corpus := Corpus{kept: kept}
	manifest := []Claim{
		{Doc: "08-claims-and-identity.md", Text: kept, Src: kept},
		{Doc: "06-issue-commands.md", Text: "on your path", Src: "blocked on %s (unclaimed, on your path)"},
	}
	missing := Missing(manifest, corpus)
	if len(missing) != 1 {
		t.Fatalf("Missing() reported %d claims, want exactly 1 — the gate cannot see a dropped message", len(missing))
	}
	if missing[0].Text != "on your path" {
		t.Errorf("Missing() reported %q, want the dropped claim", missing[0].Text)
	}
}

// TestMissingSeesThroughACoincidentalSubstring is the reason a claim records
// the source it was found in. Matching against the union of all shipped text is
// far too weak: 269 of this corpus's 1,102 entries have text sitting inside two
// or more distinct sources (measured 2026-09-18). Here the documented message is deleted and an unrelated one still
// contains its words.
func TestMissingSeesThroughACoincidentalSubstring(t *testing.T) {
	corpus := Corpus{"some other sentence about deleted_at IS NULL here": "some other sentence about deleted_at IS NULL here"}
	manifest := []Claim{{
		Doc:  "03-store-schema.md",
		Text: "deleted_at IS NULL",
		Src:  "SELECT id FROM issues WHERE deleted_at IS NULL",
	}}
	if missing := Missing(manifest, corpus); len(missing) != 1 {
		t.Fatal("a deleted message was masked by a coincidental substring in an unrelated source")
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
	}
	corpus, err := ShippedText(fsys)
	if err != nil {
		t.Fatalf("ShippedText: %v", err)
	}
	for _, want := range []string{"shipped message here", "vendored linked message"} {
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
	} {
		if _, ok := corpus[absent]; ok {
			t.Errorf("%q counted as shipped; nothing links it", absent)
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
