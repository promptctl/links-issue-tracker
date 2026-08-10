package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// unpushedCloneWithOneLocalChange stands up a bare remote and a single clone
// that has pushed once (establishing the tracking ref) and then created a
// ticket it has NOT pushed — the minimal "N local change(s) not pushed"
// fixture links-sync-pgct.2's acceptance criterion names directly. Auto-sync
// is disabled so the caller controls exactly when (and whether) the pending
// change gets pushed.
func unpushedCloneWithOneLocalChange(t *testing.T) (dir, ticketID string) {
	t.Helper()
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	dir = filepath.Join(base, "solo")
	runGit(t, base, "clone", remote, "solo")
	runGit(t, dir, "config", "user.email", "solo@example.com")
	runGit(t, dir, "config", "user.name", "solo")
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "seed")
	runGit(t, dir, "push", "origin", "HEAD")

	t.Setenv(DisableAutoSyncEnvVar, "1")
	runCLIInDir(t, dir, "init", "--skip-hooks", "--skip-agents")
	// Establish the remote-tracking ref (Synced=true) with nothing pending, so
	// the ticket created below is the ONLY unpushed change — otherwise the
	// freshly-created store reads SyncNeverSynced, not SyncAhead.
	runCLIInDir(t, dir, "sync", "push", "--set-upstream")

	ticketID = extractTicketID(t, runCLIInDir(t, dir, "new",
		"--title", "unpushed-ticket", "--description", "d", "--topic", "demo", "--type", "task"))
	return dir, ticketID
}

// TestBacklogNextShowWarnOnUnpushedChangesAndClearAfterPush drives the exact
// acceptance criterion in links-sync-pgct.2: "with local changes unpushed,
// running the ordinary read commands shows the warning; after a successful
// push it disappears" — across all three commands the ticket names
// explicitly (backlog, next, show), through the real CLI entrypoint.
func TestBacklogNextShowWarnOnUnpushedChangesAndClearAfterPush(t *testing.T) {
	dir, ticketID := unpushedCloneWithOneLocalChange(t)

	backlogOut := runCLIInDir(t, dir, "backlog")
	if !strings.Contains(backlogOut, "sync:") || !strings.Contains(backlogOut, "not pushed") || !strings.Contains(backlogOut, "lit sync push") {
		t.Fatalf("`lit backlog` with an unpushed change did not warn:\n%s", backlogOut)
	}

	nextOut := runCLIInDir(t, dir, "next")
	if !strings.Contains(nextOut, "sync:") || !strings.Contains(nextOut, "not pushed") {
		t.Fatalf("`lit next` with an unpushed change did not warn:\n%s", nextOut)
	}
	if !strings.Contains(nextOut, ticketID) {
		t.Fatalf("`lit next` warning apparently replaced the ticket summary instead of preceding it:\n%s", nextOut)
	}

	showOut := runCLIInDir(t, dir, "show", ticketID)
	if !strings.Contains(showOut, "sync:") || !strings.Contains(showOut, "not pushed") {
		t.Fatalf("`lit show` with an unpushed change did not warn:\n%s", showOut)
	}
	if !strings.Contains(showOut, ticketID) {
		t.Fatalf("`lit show` output missing the ticket it was asked to show:\n%s", showOut)
	}

	runCLIInDir(t, dir, "sync", "push")

	backlogAfterPush := runCLIInDir(t, dir, "backlog")
	if strings.Contains(backlogAfterPush, "not pushed") {
		t.Fatalf("`lit backlog` still warns after a successful push:\n%s", backlogAfterPush)
	}
}

// TestBacklogWarnsOnStaleFetchAndClearsAfterFetch drives the ticket's second,
// independent condition end to end — "the last fetch is older than a
// threshold" — through the real fetch-success marker wiring: the marker is
// backdated directly (the pure-logic table in TestSyncStalenessLines already
// pins that a stale-fetch warning fires independently of the ahead count, so
// this test does not also try to force the store into a specific ahead
// state), then a real `lit sync fetch` must both clear the warning and
// refresh the marker.
func TestBacklogWarnsOnStaleFetchAndClearsAfterFetch(t *testing.T) {
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	dir := filepath.Join(base, "solo")
	runGit(t, base, "clone", remote, "solo")
	runGit(t, dir, "config", "user.email", "solo@example.com")
	runGit(t, dir, "config", "user.name", "solo")
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "seed")
	runGit(t, dir, "push", "origin", "HEAD")

	t.Setenv(DisableAutoSyncEnvVar, "1")
	runCLIInDir(t, dir, "init", "--skip-hooks", "--skip-agents")
	runCLIInDir(t, dir, "sync", "push", "--set-upstream")

	ws, err := workspace.Resolve(dir)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	// The push above never fetches, so no fetch-success marker exists yet —
	// create one, then backdate it past the threshold.
	if err := markFetchSuccess(ws); err != nil {
		t.Fatalf("markFetchSuccess() error = %v", err)
	}
	backdated := time.Now().Add(-30 * time.Hour)
	if err := os.Chtimes(fetchSuccessMarkerPath(ws), backdated, backdated); err != nil {
		t.Fatalf("os.Chtimes() error = %v", err)
	}

	backlogOut := runCLIInDir(t, dir, "backlog")
	if !strings.Contains(backlogOut, "sync:") || !strings.Contains(backlogOut, "last successful fetch") || !strings.Contains(backlogOut, "lit sync fetch") {
		t.Fatalf("`lit backlog` with a stale fetch marker did not warn:\n%s", backlogOut)
	}

	runCLIInDir(t, dir, "sync", "fetch")

	backlogAfterFetch := runCLIInDir(t, dir, "backlog")
	if strings.Contains(backlogAfterFetch, "last successful fetch") {
		t.Fatalf("`lit backlog` still warns about a stale fetch after `lit sync fetch`:\n%s", backlogAfterFetch)
	}
}
