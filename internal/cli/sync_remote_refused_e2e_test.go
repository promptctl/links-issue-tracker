package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestAGitRemoteDoltCannotHoldIsRefusedAndStopsNoOtherSync gives a checkout
// syncing with remote a a second git remote, a/b, whose name Dolt refuses. The
// reconcile mirrors a and refuses a/b, so syncing with a goes on; the event
// and the reports name the refusal, and naming a/b as the sync target fails
// with the reason.
func TestAGitRemoteDoltCannotHoldIsRefusedAndStopsNoOtherSync(t *testing.T) {
	isolateEvents(t)
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	dir := filepath.Join(base, "solo")
	runGit(t, base, "clone", filepath.Join(base, "remote.git"), "solo")
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
	if refused := lastEvent(t).Facts["remotes.refused"]; !reflect.DeepEqual(refused, []any{}) {
		t.Fatalf("remotes.refused with a alone = %#v, want []", refused)
	}

	runGit(t, dir, "remote", "add", "a/b", filepath.Join(base, "remote.git"))
	if out := runCLIInDir(t, dir, "sync", "fetch", "--verbose"); strings.TrimSpace(out) != "fetched a" {
		t.Fatalf("`lit sync fetch --verbose` beside a/b = %q, want %q", out, "fetched a")
	}
	if refused := lastEvent(t).Facts["remotes.refused"]; !reflect.DeepEqual(refused, []any{"a/b"}) {
		t.Fatalf("remotes.refused = %#v, want [a/b]", refused)
	}
	if out := runCLIInDir(t, dir, "sync", "remote", "ls"); strings.TrimSpace(out) != "git=2 dolt=1 added=0 updated=0 removed=0 refused=1" {
		t.Fatalf("`lit sync remote ls` = %q, want a/b refused and a mirrored", out)
	}

	out, err := runCLIInDirAllowError(t, dir, "sync", "fetch", "--remote", "a/b")
	if err == nil {
		t.Fatalf("`lit sync fetch --remote a/b` succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), `remote "a/b" has "/" in its name`) {
		t.Fatalf("`lit sync fetch --remote a/b` error = %v, want the refusal naming a/b", err)
	}
}
