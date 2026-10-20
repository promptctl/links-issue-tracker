package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestAGitRemoteDoltCannotHoldIsRefusedAndStopsNoOtherSync gives a checkout
// syncing with remote a a second git remote, a/b, whose name Dolt refuses. The
// reconcile mirrors a and refuses a/b, so syncing with a goes on; the event
// and the reports name the refusal, and naming a/b as the sync target fails
// with the reason. A branch tracking a/b syncs with a/b, so its sync is
// refused rather than taken to a, and a fetch naming a still lands.
func TestAGitRemoteDoltCannotHoldIsRefusedAndStopsNoOtherSync(t *testing.T) {
	isolateEvents(t)
	dir, remoteURL := soloCheckoutSyncingWithA(t)
	if refused := lastEvent(t).Facts["remotes.refused"]; !reflect.DeepEqual(refused, []any{}) {
		t.Fatalf("remotes.refused with a alone = %#v, want []", refused)
	}

	runGit(t, dir, "remote", "add", "a/b", remoteURL)
	if out := runCLIInDir(t, dir, "sync", "fetch", "--verbose"); strings.TrimSpace(out) != "fetched a" {
		t.Fatalf("`lit sync fetch --verbose` beside a/b = %q, want %q", out, "fetched a")
	}
	if refused := lastEvent(t).Facts["remotes.refused"]; !reflect.DeepEqual(refused, []any{"a/b"}) {
		t.Fatalf("remotes.refused = %#v, want [a/b]", refused)
	}
	if out := runCLIInDir(t, dir, "sync", "remote", "ls"); strings.TrimSpace(out) != "git=2 dolt=1 added=0 updated=0 removed=0 refused=1\nrefused a/b: Dolt cannot hold this name; rename it (git remote rename) to sync with it" {
		t.Fatalf("`lit sync remote ls` = %q, want a/b refused and a mirrored", out)
	}

	out, err := runCLIInDirAllowError(t, dir, "sync", "fetch", "--remote", "a/b")
	if err == nil {
		t.Fatalf("`lit sync fetch --remote a/b` succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), `remote "a/b" has "/" in its name`) {
		t.Fatalf("`lit sync fetch --remote a/b` error = %v, want the refusal naming a/b", err)
	}

	runGit(t, dir, "push", "--set-upstream", "a/b", "HEAD")
	if out, err := runCLIInDirAllowError(t, dir, "sync", "fetch", "--verbose"); err == nil || !strings.Contains(err.Error(), `remote "a/b" has "/" in its name`) {
		t.Fatalf("`lit sync fetch` tracking a/b = %q, %v; want the refusal naming a/b", out, err)
	}
	if out := runCLIInDir(t, dir, "sync", "fetch", "--remote", "a", "--verbose"); strings.TrimSpace(out) != "fetched a" {
		t.Fatalf("`lit sync fetch --remote a` tracking a/b = %q, want %q", out, "fetched a")
	}

	// The automatic receive ends on the refusal before it clones the store,
	// with the one trace that names it.
	ws, err := workspace.Resolve(dir)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	before := len(readSyncTraceRecords(t, ws))
	receiveNow(t, dir)
	records := readSyncTraceRecords(t, ws)[before:]
	if len(records) != 1 || records[0].Decision != "error" || !strings.Contains(records[0].Reason, `remote "a/b" has "/" in its name`) {
		t.Fatalf("receive tracking a/b traced %+v; want one error naming the refusal", records)
	}
}

// TestRemovingAGitRemoteRecordsItsRemoval: a remote git no longer names
// leaves Dolt at the next sync, and that sync's event records the removal
// and the tracking refs that went with it.
func TestRemovingAGitRemoteRecordsItsRemoval(t *testing.T) {
	isolateEvents(t)
	dir, remoteURL := soloCheckoutSyncingWithA(t)
	runGit(t, dir, "remote", "add", "c", remoteURL)
	runCLIInDir(t, dir, "sync", "fetch", "--remote", "c")
	if removals, ok := lastEvent(t).Occurrences["remote.removals"]; !ok || len(removals) != 0 {
		t.Fatalf("remote.removals with nothing to remove = %#v, want []", removals)
	}

	runGit(t, dir, "remote", "remove", "c")
	runCLIInDir(t, dir, "sync", "fetch")
	want := []any{map[string]any{
		"remote_removal.remote":                "c",
		"remote_removal.owner":                 "c",
		"remote_removal.held":                  true,
		"remote_removal.owner_held":            false,
		"remote_removal.tracking_refs_dropped": float64(1),
	}}
	if removals := lastEvent(t).Occurrences["remote.removals"]; !reflect.DeepEqual(removals, want) {
		t.Fatalf("remote.removals = %#v, want %#v", removals, want)
	}
}

// soloCheckoutSyncingWithA is a lit checkout whose one git remote, a, holds
// its pushed store, and the URL of a's repository.
func soloCheckoutSyncingWithA(t *testing.T) (dir, remoteURL string) {
	t.Helper()
	base := t.TempDir()
	remoteURL = filepath.Join(base, "remote.git")
	runGit(t, base, "init", "--bare", "remote.git")
	dir = filepath.Join(base, "solo")
	runGit(t, base, "clone", remoteURL, "solo")
	runGit(t, dir, "config", "user.email", "solo@example.com")
	runGit(t, dir, "config", "user.name", "solo")
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "seed")
	runGit(t, dir, "push", "origin", "HEAD")
	runGit(t, dir, "remote", "rename", "origin", "a")

	t.Setenv(DisableAutoSyncEnvVar, "1")
	runCLIInDir(t, dir, "init", "--skip-hooks", "--skip-agents")
	runCLIInDir(t, dir, "sync", "push", "--set-upstream")
	return dir, remoteURL
}
