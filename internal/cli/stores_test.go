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
	wantA, _ := filepath.EvalSymlinks(filepath.Join(repoA, ".git"))
	wantB, _ := filepath.EvalSymlinks(filepath.Join(repoB, ".git"))
	if lines[0] != filepath.Join(wantA, "links") || lines[1] != filepath.Join(wantB, "links") {
		t.Fatalf("runStores() output = %q, want repoA then repoB store dirs", lines)
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
