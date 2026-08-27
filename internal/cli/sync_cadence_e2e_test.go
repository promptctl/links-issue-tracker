package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The compaction backstop is wired into maybeAutoSyncAfterCommand behind a
// single condition — accessMode == app.AccessWrite — and that condition is
// invisible to the rest of this package's suite: TestMain sets
// LIT_DISABLE_AUTO_SYNC=1 for every test in it, so the cadence owner never runs
// and an inverted gate would sail through a fully green package. These tests
// turn it back on deliberately and assert the gate from outside, through the
// real CLI entrypoint.
//
// Three arms, because there are three ways the gate can be wrong and each is
// silent: it could fail to fire on a write (the feature simply never happens),
// fire on a read (a store-rewriting pass behind `lit backlog`), or ignore the
// opt-out (a workspace that disabled automatic sync still gets automatic
// maintenance). [LAW:behavior-not-structure] each asserts an observable — the
// probe marker on disk — never how the cadence owner reaches its decision.

// litRepoForCadence stands up a real git repo with lit initialized in it, and
// returns the directory plus the backstop's probe-marker path. The marker's
// name comes from compactMarkerPath, the production derivation, so a rename
// there fails these tests rather than quietly making them assert a file nobody
// writes.
func litRepoForCadence(t *testing.T) (dir, marker string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "repo")
	runGit(t, base, "init", "repo")
	runGit(t, dir, "config", "user.email", "cadence@example.com")
	runGit(t, dir, "config", "user.name", "cadence")
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "seed")

	// init runs with auto-sync still disabled by TestMain, so the workspace
	// starts with no probe marker and each test below observes only what its
	// own command did.
	runCLIInDir(t, dir, "init", "--skip-hooks", "--skip-agents")

	ws := workspace.Info{Location: workspace.LocationFromStorageDir(filepath.Join(dir, ".git", "links"))}
	marker = compactMarkerPath(ws)
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("probe marker existed before any auto-synced command: %s", marker)
	}
	return dir, marker
}

func newTicket(t *testing.T, dir string) {
	t.Helper()
	runCLIInDir(t, dir, "new",
		"--title", "cadence-ticket", "--description", "d", "--topic", "demo", "--type", "task")
}

// A mutating command runs the backstop: only a mutation grows the store, and a
// workspace with no remote is exactly the one with nothing else to collect it.
func TestWriteCommandRunsTheCompactionBackstop(t *testing.T) {
	dir, marker := litRepoForCadence(t)
	t.Setenv(DisableAutoSyncEnvVar, "0")

	newTicket(t, dir)

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("no compaction probe after a write command (%v); the backstop never ran, and nothing else would have said so", err)
	}
}

// A read-only command does not. Compaction rewrites the store, so hanging it on
// any command rather than on having written would put that work behind
// `lit backlog`.
func TestReadOnlyCommandDoesNotRunTheCompactionBackstop(t *testing.T) {
	dir, marker := litRepoForCadence(t)
	t.Setenv(DisableAutoSyncEnvVar, "0")

	runCLIInDir(t, dir, "backlog")

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a read-only command left a compaction probe; the backstop is gated on having written, not on any command running")
	}
}

// The opt-out covers maintenance too. It is checked before the cadence owner
// does anything, and every other test in this package depends on that holding —
// TestMain relies on this one switch to keep automatic work out of unrelated
// tests.
func TestDisabledAutoSyncSuppressesTheCompactionBackstop(t *testing.T) {
	dir, marker := litRepoForCadence(t)
	t.Setenv(DisableAutoSyncEnvVar, "1")

	newTicket(t, dir)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the backstop ran with automatic sync disabled; the opt-out must cover automatic maintenance as well as automatic sync")
	}
}
