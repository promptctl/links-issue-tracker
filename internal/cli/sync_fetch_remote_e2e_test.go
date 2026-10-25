package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestSyncFetchSelectsTheSyncRemoteAndFailsOnAnUnreadableBranchRecord drives
// `lit sync fetch` in a workspace whose only remote is not named origin. With
// no --remote it fetches that remote, as pull and push select it. Then, with
// nothing in git naming the remote's default branch and the record that would
// name it unreadable, the fetch fails rather than skipping the re-root follow
// as if no branch were known.
func TestSyncFetchSelectsTheSyncRemoteAndFailsOnAnUnreadableBranchRecord(t *testing.T) {
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
	runGit(t, dir, "remote", "rename", "origin", "gh")

	t.Setenv(DisableAutoSyncEnvVar, "1")
	runCLIInDir(t, dir, "init", "--skip-hooks")
	runCLIInDir(t, dir, "sync", "push", "--set-upstream")

	if out := runCLIInDir(t, dir, "sync", "fetch", "--verbose"); strings.TrimSpace(out) != "fetched gh" {
		t.Fatalf("`lit sync fetch --verbose` = %q, want %q", out, "fetched gh")
	}

	ws, err := workspace.Resolve(testEnv(), dir)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	runGit(t, dir, "remote", "set-head", "gh", "--delete")
	record := advertisedSyncBranchPath(ws, remoteName(t, "gh"))
	if err := os.RemoveAll(record); err != nil {
		t.Fatalf("remove sync branch record: %v", err)
	}
	if err := os.Mkdir(record, 0o755); err != nil {
		t.Fatalf("make sync branch record unreadable: %v", err)
	}
	out, err := runCLIInDirAllowError(t, dir, "sync", "fetch")
	if err == nil {
		t.Fatalf("`lit sync fetch` with an unreadable sync branch record succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("`lit sync fetch` error = %v, want the unreadable record named", err)
	}

	// Two remotes and no upstream select none: the fetch says it skipped,
	// without --verbose, since the staleness banner sends people to it.
	runGit(t, dir, "branch", "--unset-upstream")
	runGit(t, dir, "remote", "add", "other", filepath.Join(base, "remote.git"))
	if out := runCLIInDir(t, dir, "sync", "fetch"); strings.TrimSpace(out) != "skipped sync fetch: no eligible git remote; name one with --remote" {
		t.Fatalf("`lit sync fetch` with no remote selected = %q, want the skip said", out)
	}
}
