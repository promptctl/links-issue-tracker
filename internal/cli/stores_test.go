package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init")
}

func addLitStore(t *testing.T, repoDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git", "links", "dolt"), 0o755); err != nil {
		t.Fatalf("addLitStore(%q) error = %v", repoDir, err)
	}
}

// TestRunStoresListsDiscoveredStores drives the command surface: given an
// explicit root over a tree with two lit repos and one lit-less git repo, it
// prints exactly the two store directories, sorted, one per line.
func TestRunStoresListsDiscoveredStores(t *testing.T) {
	root := t.TempDir()

	repoA := filepath.Join(root, "repoA")
	if err := os.MkdirAll(repoA, 0o755); err != nil {
		t.Fatalf("mkdir repoA: %v", err)
	}
	gitInit(t, repoA)
	addLitStore(t, repoA)

	repoB := filepath.Join(root, "repoB")
	if err := os.MkdirAll(repoB, 0o755); err != nil {
		t.Fatalf("mkdir repoB: %v", err)
	}
	gitInit(t, repoB)
	addLitStore(t, repoB)

	gitOnly := filepath.Join(root, "gitOnly")
	if err := os.MkdirAll(gitOnly, 0o755); err != nil {
		t.Fatalf("mkdir gitOnly: %v", err)
	}
	gitInit(t, gitOnly)

	var out bytes.Buffer
	if err := runStores(&out, []string{root}); err != nil {
		t.Fatalf("runStores() error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("runStores() printed %d lines, want 2:\n%s", len(lines), out.String())
	}
	wantA, err := filepath.EvalSymlinks(filepath.Join(repoA, ".git"))
	if err != nil {
		t.Fatalf("EvalSymlinks(repoA/.git) error = %v", err)
	}
	wantB, err := filepath.EvalSymlinks(filepath.Join(repoB, ".git"))
	if err != nil {
		t.Fatalf("EvalSymlinks(repoB/.git) error = %v", err)
	}
	if lines[0] != filepath.Join(wantA, "links") || lines[1] != filepath.Join(wantB, "links") {
		t.Fatalf("runStores() output = %q, want repoA then repoB store dirs", lines)
	}
}

// TestRunStoresPropagatesDiscoverError pins the error contract: when Discover
// fails, runStores returns the error rather than printing an empty success. A
// non-existent root makes the filesystem walk fail deterministically, without
// coupling the test to git-resolution mechanics. [LAW:behavior-not-structure]
func TestRunStoresPropagatesDiscoverError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	var out bytes.Buffer
	err := runStores(&out, []string{missing})
	if err == nil {
		t.Fatalf("runStores() returned nil error with output %q; want a surfaced Discover failure", out.String())
	}
	if out.Len() != 0 {
		t.Fatalf("runStores() emitted %q before failing; want no output on the error path", out.String())
	}
}

// TestRunStoresEmptyWhenNoStores confirms a store-less root exits cleanly with
// no output rather than erroring.
func TestRunStoresEmptyWhenNoStores(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "plain"), 0o755); err != nil {
		t.Fatalf("mkdir plain: %v", err)
	}

	var out bytes.Buffer
	if err := runStores(&out, []string{root}); err != nil {
		t.Fatalf("runStores() error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("runStores() output = %q, want empty", out.String())
	}
}

// TestPrintCrossProjectRollupTableAndErrors pins the render contract: readable
// projects become count rows summed into a TOTAL, and an unreadable project
// renders as a marked error row AFTER the table without disturbing the counts —
// the criterion's "error row while the other projects still render".
// [LAW:behavior-not-structure] Asserts the emitted view, not how it was built.
func TestPrintCrossProjectRollupTableAndErrors(t *testing.T) {
	rows := []projectRollup{
		{Label: "alpha", StorageDir: "/repos/alpha/.git/links", Ready: 2, InFlight: 1, Blocked: 3},
		{Label: "/repos/broken/.git/links", StorageDir: "/repos/broken/.git/links", Err: errors.New("open store: manifest missing")},
		{Label: "beta", StorageDir: "/repos/beta/.git/links", Ready: 4, InFlight: 0, Blocked: 1},
	}

	var out bytes.Buffer
	if err := printCrossProjectRollup(&out, rows); err != nil {
		t.Fatalf("printCrossProjectRollup() error = %v", err)
	}
	got := out.String()

	for _, want := range []string{"PROJECT", "READY", "IN-FLIGHT", "BLOCKED"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing header %q:\n%s", want, got)
		}
	}
	// Both readable projects render...
	if !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
		t.Fatalf("output missing a readable project row:\n%s", got)
	}
	// ...the TOTAL sums only the readable projects (2+4 ready, 1+0 in-flight, 3+1
	// blocked), the broken one contributing nothing...
	if !strings.Contains(got, "TOTAL") {
		t.Fatalf("output missing TOTAL row:\n%s", got)
	}
	totalLine := lineContaining(t, got, "TOTAL")
	for _, want := range []string{"6", "1", "4"} {
		if !strings.Contains(totalLine, want) {
			t.Fatalf("TOTAL line %q missing aggregate count %q", totalLine, want)
		}
	}
	// ...and the broken store is a loud, marked error row naming its path and cause.
	errLine := lineContaining(t, got, "manifest missing")
	if !strings.HasPrefix(strings.TrimSpace(errLine), "!") {
		t.Fatalf("error row %q is not marked with a leading '!'", errLine)
	}
	if !strings.Contains(errLine, "/repos/broken/.git/links") {
		t.Fatalf("error row %q does not name the unreadable store path", errLine)
	}
}

// TestGatherCrossProjectRollupUnreadableStoreIsErrorRow drives the gather path:
// a tree of two discovered-but-unopenable stores yields two error rows rather
// than a fatal error, so one broken store never aborts the whole overview.
// [LAW:no-silent-failure] The stores are surfaced as errors, not skipped.
func TestGatherCrossProjectRollupUnreadableStoreIsErrorRow(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"repoA", "repoB"} {
		repo := filepath.Join(root, name)
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		gitInit(t, repo)
		addLitStore(t, repo) // a store DIRECTORY exists but holds no real dolt data
	}

	rows, err := gatherCrossProjectRollup(context.Background(), []string{root})
	if err != nil {
		t.Fatalf("gatherCrossProjectRollup() error = %v; want error rows, not a fatal error", err)
	}
	if len(rows) != 2 {
		t.Fatalf("gatherCrossProjectRollup() returned %d rows, want 2:\n%+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Err == nil {
			t.Fatalf("row %q has nil Err; an empty store dir must not open cleanly", row.StorageDir)
		}
	}
}

// lineContaining returns the single output line that contains sub, failing the
// test if none does — a small helper so the render assertions read as intent.
func lineContaining(t *testing.T, out, sub string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	t.Fatalf("no output line contains %q:\n%s", sub, out)
	return ""
}
