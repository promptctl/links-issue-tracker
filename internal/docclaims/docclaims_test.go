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
// the code that ships them. This is that comparison.
//
// A failure here is not a broken test: it is a chapter describing a message the
// binary no longer has. Fix the prose, then regenerate the manifest with
// `go run ./tools/docclaims-sync`.
func TestDocumentedClaimsStillShip(t *testing.T) {
	shipped, err := ShippedLiterals(os.DirFS(repoRoot))
	if err != nil {
		t.Fatalf("ShippedLiterals: %v", err)
	}
	if len(Manifest) == 0 {
		t.Fatal("manifest is empty — the gate would pass over anything; regenerate with `go run ./tools/docclaims-sync`")
	}
	missing := Missing(Manifest, shipped)
	for _, c := range missing {
		t.Errorf("%s quotes a message that no longer ships:\n  %q\nrecorded in the literal:\n  %q\nEither the prose is now false and should be corrected, or the message moved deliberately and the manifest needs `go run ./tools/docclaims-sync`.", c.Doc, c.Text, c.Lit)
	}
	if len(missing) > 0 {
		t.Logf("%d of %d documented literals have drifted", len(missing), len(Manifest))
	}
}

// TestManifestIsCurrent fails when the committed manifest disagrees with what
// this tree yields.
//
// Without it nothing compares the two, which is how a manifest derived over a
// working tree carrying a gitignored vendored project was committed: 14 of its
// entries were satisfied only by that project's literals, and the gate passed
// locally while failing in every clean checkout.
func TestManifestIsCurrent(t *testing.T) {
	fsys := os.DirFS(repoRoot)
	shipped, err := ShippedLiterals(fsys)
	if err != nil {
		t.Fatalf("ShippedLiterals: %v", err)
	}
	names, err := SpecFiles(fsys)
	if err != nil {
		t.Fatalf("SpecFiles: %v", err)
	}
	claims, err := DocClaims(fsys, names)
	if err != nil {
		t.Fatalf("DocClaims: %v", err)
	}
	fresh := Dedupe(Matched(claims, shipped))
	if len(fresh) != len(Manifest) {
		t.Fatalf("manifest holds %d literals, this tree yields %d — run `go run ./tools/docclaims-sync`", len(Manifest), len(fresh))
	}
	recorded := map[Claim]bool{}
	for _, c := range Manifest {
		recorded[c] = true
	}
	for _, c := range fresh {
		if !recorded[c] {
			t.Errorf("not in the committed manifest: %s %q — run `go run ./tools/docclaims-sync`", c.Doc, c.Text)
		}
	}
}

// TestMissingReportsADroppedLiteral is the mutation control on the gate.
//
// "Every manifest entry still ships" passes trivially if Missing can never
// report anything — an empty manifest, or a containment check that matches
// everything. So the adversarial case is built by hand.
func TestMissingReportsADroppedLiteral(t *testing.T) {
	kept := "no ready work"
	shipped := map[string]bool{kept: true}
	manifest := []Claim{
		{Doc: "08-claims-and-identity.md", Text: kept, Lit: kept},
		{Doc: "06-issue-commands.md", Text: "on your path", Lit: "blocked on %s (unclaimed, on your path)"},
	}
	missing := Missing(manifest, shipped)
	if len(missing) != 1 {
		t.Fatalf("Missing() reported %d claims, want exactly 1 — the gate cannot see a dropped literal", len(missing))
	}
	if missing[0].Text != "on your path" {
		t.Errorf("Missing() reported %q, want the dropped claim", missing[0].Text)
	}
}

// TestMissingSeesThroughACoincidentalSubstring is the reason a claim records
// the literal it was found in.
//
// Matching a quotation against the union of every shipped literal is far too
// weak: 174 of this corpus's 1,054 quotations sit inside two or more distinct
// literals. Here the documented message is deleted and an unrelated one still
// contains its words — under a union match the gate stays green, which is the
// exact silence this package was written to end.
func TestMissingSeesThroughACoincidentalSubstring(t *testing.T) {
	shipped := map[string]bool{"some other sentence about deleted_at IS NULL here": true}
	manifest := []Claim{{
		Doc:  "03-store-schema.md",
		Text: "deleted_at IS NULL",
		Lit:  "SELECT id FROM issues WHERE deleted_at IS NULL",
	}}
	if missing := Missing(manifest, shipped); len(missing) != 1 {
		t.Fatal("a deleted message was masked by a coincidental substring in an unrelated literal")
	}
}

// TestShippedLiteralsReadsOnlyProductCode pins the corpus. A literal living in
// a test file, under tools/, or in an untracked vendored tree must not count as
// shipped: each would let a documented message survive its own deletion from
// the product.
func TestShippedLiteralsReadsOnlyProductCode(t *testing.T) {
	fsys := fstest.MapFS{
		"cmd/lit/main.go":            {Data: []byte("package main\nvar A = \"shipped message here\"\n")},
		"internal/cli/real.go":       {Data: []byte("package cli\nvar B = \"another shipped message\"\n")},
		"internal/cli/real_test.go":  {Data: []byte("package cli\nvar C = \"test only message\"\n")},
		"internal/cli/testdata/x.go": {Data: []byte("package cli\nvar D = \"testdata only message\"\n")},
		"tools/thing/main.go":        {Data: []byte("package main\nvar E = \"tool only message\"\n")},
		"artifacts/beads/b.go":       {Data: []byte("package beads\nvar F = \"vendored only message\"\n")},
	}
	shipped, err := ShippedLiterals(fsys)
	if err != nil {
		t.Fatalf("ShippedLiterals: %v", err)
	}
	for _, want := range []string{"shipped message here", "another shipped message"} {
		if !shipped[want] {
			t.Errorf("%q ships but was not collected", want)
		}
	}
	for _, absent := range []string{"test only message", "testdata only message", "tool only message", "vendored only message"} {
		if shipped[absent] {
			t.Errorf("%q counted as shipped; it does not ship", absent)
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
	got := spansIn(src)
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
