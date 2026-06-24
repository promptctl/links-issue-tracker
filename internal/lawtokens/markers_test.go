package lawtokens

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// brackets wraps a namespace and token into a citation at runtime. Tests build
// their example markers through this rather than writing literal "[LAW:...]"
// text, so that the repo gate (TestRepoMarkersAreCanonical) — which scans every
// tracked file, this one included — never mistakes a deliberately-invalid test
// fixture for real drift.
func brackets(namespace, token string) string {
	return "[" + namespace + ":" + token + "]"
}

func TestScanMarkersRecognizesShapeRegardlessOfCanonicity(t *testing.T) {
	canonical := brackets("LAW", "single-enforcer")
	invalidToken := brackets("LAW", "no-silent-fallbacks")
	wrongNamespace := brackets("LAW", "representation") // representation is FRAMING
	miscased := brackets("LAW", "No-Silent-Failure")
	framing := brackets("FRAMING", "representation")

	content := strings.Join([]string{
		"// leading prose " + canonical,
		"some " + invalidToken + " mid-line and " + framing,
		"",
		"trailing " + wrongNamespace + " then " + miscased,
	}, "\n")

	got := ScanMarkers(content)

	want := []Marker{
		{Namespace: "LAW", Token: "single-enforcer", Line: 1},
		{Namespace: "LAW", Token: "no-silent-fallbacks", Line: 2},
		{Namespace: "FRAMING", Token: "representation", Line: 2},
		{Namespace: "LAW", Token: "representation", Line: 4},
		{Namespace: "LAW", Token: "No-Silent-Failure", Line: 4},
	}

	if len(got) != len(want) {
		t.Fatalf("ScanMarkers found %d markers, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("marker %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestNonCanonicalRejectsExactlyTheDrift(t *testing.T) {
	markers := ScanMarkers(strings.Join([]string{
		brackets("LAW", "single-enforcer"),     // canonical
		brackets("FRAMING", "representation"),  // canonical
		brackets("LAW", "no-silent-fallbacks"), // not in index
		brackets("LAW", "enumeration-gap"),     // a skill name, not a law
		brackets("LAW", "representation"),      // right token, wrong namespace
		brackets("LAW", "No-Silent-Failure"),   // miscased
	}, "\n"))

	bad := NonCanonical(markers)

	wantBadKeys := map[string]bool{
		"LAW:no-silent-fallbacks": true,
		"LAW:enumeration-gap":     true,
		"LAW:representation":      true,
		"LAW:No-Silent-Failure":   true,
	}
	if len(bad) != len(wantBadKeys) {
		t.Fatalf("NonCanonical returned %d markers, want %d: %+v", len(bad), len(wantBadKeys), bad)
	}
	for _, m := range bad {
		if !wantBadKeys[m.Key()] {
			t.Errorf("NonCanonical flagged %q, which should be canonical", m.Key())
		}
	}
}

func TestEveryCanonicalKeyIsAccepted(t *testing.T) {
	for _, key := range Canonical.Sorted() {
		ns, token, ok := strings.Cut(key, ":")
		if !ok {
			t.Fatalf("canonical key %q is not in NAMESPACE:token form", key)
		}
		markers := ScanMarkers(brackets(ns, token))
		if len(markers) != 1 {
			t.Fatalf("canonical key %q did not scan as exactly one marker: %+v", key, markers)
		}
		if got := NonCanonical(markers); len(got) != 0 {
			t.Errorf("canonical key %q was rejected as non-canonical", key)
		}
	}
}

// TestRepoMarkersAreCanonical is the gate ([LAW:single-enforcer],
// [LAW:verifiable-goals]): it enumerates every tracked file via `git ls-files`
// — git's index is the single source of truth for what is in the tree — scans
// each for citations, and fails loudly naming every marker whose token is
// absent from Canonical. This is the deterministic check that makes the
// invented-token class of cleanup ticket unable to recur.
func TestRepoMarkersAreCanonical(t *testing.T) {
	root := repoRoot(t)

	out, err := runGit(root, "ls-files", "-z")
	if err != nil {
		t.Fatalf("git ls-files failed in %s: %v", root, err)
	}
	files := splitNUL(out)
	if len(files) == 0 {
		t.Fatalf("git ls-files returned no tracked files in %s — cannot verify markers", root)
	}

	var violations []string
	for _, rel := range files {
		abs := filepath.Join(root, rel)
		content, err := os.ReadFile(abs)
		if err != nil {
			// A tracked file that cannot be read is a real fault, not something
			// to skip past ([LAW:no-silent-failure]).
			t.Fatalf("reading tracked file %s: %v", rel, err)
		}
		for _, m := range NonCanonical(ScanMarkers(string(content))) {
			violations = append(violations, rel+":"+strconv.Itoa(m.Line)+": "+m.String())
		}
	}

	if len(violations) > 0 {
		t.Fatalf("found %d non-canonical [LAW]/[FRAMING] marker(s) — every token must "+
			"appear in lawtokens.Canonical (the universal-laws Token index). Fix the token "+
			"or, if it is genuinely a new law, add it to Canonical first:\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := runGit(".", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("locating repo root via git: %v", err)
	}
	return strings.TrimSpace(out)
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

func splitNUL(s string) []string {
	var parts []string
	for _, p := range strings.Split(s, "\x00") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}
