package cli

import (
	"bytes"
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
