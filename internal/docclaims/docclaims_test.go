package docclaims

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
		t.Errorf("%s quotes a message that no longer ships:\n  %q\nEither the prose is now false and should be corrected, or the message moved deliberately and the manifest needs `go run ./tools/docclaims-sync`.", c.Doc, c.Text)
	}
	if len(missing) > 0 {
		t.Logf("%d of %d documented literals have drifted", len(missing), len(Manifest))
	}
}

// TestMissingReportsADroppedLiteral is the mutation control on the test above.
//
// "Every manifest entry still ships" passes trivially if Missing can never
// report anything — an empty manifest, or a containment check that matches
// everything. So the adversarial case is built by hand: a documented string
// that is deliberately absent from the shipped set must be reported.
func TestMissingReportsADroppedLiteral(t *testing.T) {
	shipped := map[string]bool{"no ready work": true}
	manifest := []Claim{
		{Doc: "08-claims-and-identity.md", Text: "no ready work"},
		{Doc: "06-issue-commands.md", Text: "blocked on %s (unclaimed, on your path"},
	}
	missing := Missing(manifest, shipped)
	if len(missing) != 1 {
		t.Fatalf("Missing() reported %d claims, want exactly 1 — the gate cannot see a dropped literal", len(missing))
	}
	if !strings.HasPrefix(missing[0].Text, "blocked on") {
		t.Errorf("Missing() reported %q, want the dropped claim", missing[0].Text)
	}
}

// TestShippedLiteralsExcludesWhatDoesNotShip pins the scope. A literal living
// only in a test file or under tools/ must not count as shipped: if it did, a
// documented message could survive its own deletion from the product, which is
// the exact silence this package was written to end.
func TestShippedLiteralsExcludesWhatDoesNotShip(t *testing.T) {
	fsys := fstest.MapFS{
		"internal/cli/real.go":       {Data: []byte("package cli\nvar A = \"shipped message here\"\n")},
		"internal/cli/real_test.go":  {Data: []byte("package cli\nvar B = \"test only message\"\n")},
		"tools/thing/main.go":        {Data: []byte("package main\nvar C = \"tool only message\"\n")},
		".claude/worktrees/x/own.go": {Data: []byte("package cli\nvar D = \"worktree only message\"\n")},
	}
	shipped, err := ShippedLiterals(fsys)
	if err != nil {
		t.Fatalf("ShippedLiterals: %v", err)
	}
	if !shipped["shipped message here"] {
		t.Error("a literal in shipped code was not collected")
	}
	for _, absent := range []string{"test only message", "tool only message", "worktree only message"} {
		if shipped[absent] {
			t.Errorf("%q counted as shipped; it does not ship", absent)
		}
	}
}

// TestSpansInReadsBothQuotingShapes covers the corpus's two backtick shapes. A
// single-backtick pattern alone cuts a “…“ span at its inner backtick and
// protects a fragment instead of the message, which is how a checker ends up
// green over text it never really read.
func TestSpansInReadsBothQuotingShapes(t *testing.T) {
	src := "Both exit 6 with ``no ready work in %s — not a bare `next` `` and `the backlog is not empty` here.\n" +
		"Short: `a b` and one-word: `NoWork` are not claims.\n"
	got := spansIn(src)
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "no ready work in %s") {
		t.Errorf("double-backtick span not read; got %q", joined)
	}
	if !strings.Contains(joined, "the backlog is not empty") {
		t.Errorf("single-backtick span not read; got %q", joined)
	}
	for _, rejected := range []string{"a b", "NoWork"} {
		for _, s := range got {
			if s == rejected {
				t.Errorf("%q was kept as a claim; too short or single-word to be a quoted message", rejected)
			}
		}
	}
}
