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
// syncing with remote a a second git remote, team/fork, whose name Dolt
// refuses. The reconcile mirrors a and refuses team/fork, so syncing with a
// goes on; the event and the reports name the refusal, and naming team/fork
// as the sync target fails with the reason. A branch tracking team/fork syncs
// with team/fork, so its sync is refused rather than read as some other
// remote's, and a fetch naming a still lands.
func TestAGitRemoteDoltCannotHoldIsRefusedAndStopsNoOtherSync(t *testing.T) {
	isolateEvents(t)
	dir, remoteURL := soloCheckoutSyncingWithA(t)
	if refused := lastEvent(t).Facts["remotes.refused"]; !reflect.DeepEqual(refused, []any{}) {
		t.Fatalf("remotes.refused with a alone = %#v, want []", refused)
	}

	runGit(t, dir, "remote", "add", "team/fork", remoteURL)
	if out := runCLIInDir(t, dir, "sync", "fetch", "--verbose"); strings.TrimSpace(out) != "fetched a" {
		t.Fatalf("`lit sync fetch --verbose` beside team/fork = %q, want %q", out, "fetched a")
	}
	if refused := lastEvent(t).Facts["remotes.refused"]; !reflect.DeepEqual(refused, []any{"team/fork"}) {
		t.Fatalf("remotes.refused = %#v, want [team/fork]", refused)
	}
	if out := runCLIInDir(t, dir, "sync", "remote", "ls"); strings.TrimSpace(out) != "git=2 dolt=1 added=0 updated=0 removed=0 refused=1\nrefused team/fork: Dolt cannot hold this name; rename it (git remote rename) to sync with it" {
		t.Fatalf("`lit sync remote ls` = %q, want team/fork refused and a mirrored", out)
	}

	out, err := runCLIInDirAllowError(t, dir, "sync", "fetch", "--remote", "team/fork")
	if err == nil {
		t.Fatalf("`lit sync fetch --remote team/fork` succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), `remote "team/fork" has "/" in its name`) {
		t.Fatalf("`lit sync fetch --remote team/fork` error = %v, want the refusal naming team/fork", err)
	}

	runGit(t, dir, "push", "--set-upstream", "team/fork", "HEAD")
	if out, err := runCLIInDirAllowError(t, dir, "sync", "fetch", "--verbose"); err == nil || !strings.Contains(err.Error(), `remote "team/fork" has "/" in its name`) {
		t.Fatalf("`lit sync fetch` tracking team/fork = %q, %v; want the refusal naming team/fork", out, err)
	}
	if out := runCLIInDir(t, dir, "sync", "fetch", "--remote", "a", "--verbose"); strings.TrimSpace(out) != "fetched a" {
		t.Fatalf("`lit sync fetch --remote a` tracking team/fork = %q, want %q", out, "fetched a")
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
	if len(records) != 1 || records[0].Decision != "error" || !strings.Contains(records[0].Reason, `remote "team/fork" has "/" in its name`) {
		t.Fatalf("receive tracking team/fork traced %+v; want one error naming the refusal", records)
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
