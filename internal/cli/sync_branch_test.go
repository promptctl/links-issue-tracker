package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func TestResolveSyncBranchUsesDebugOverrideWhenPresent(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "debug-branch")
	ws := branchTestWorkspace(t, t.TempDir())
	got, err := resolveSyncBranch(context.Background(), ws, "origin")
	if err != nil {
		t.Fatalf("resolveSyncBranch() error = %v", err)
	}
	if got != "debug-branch" {
		t.Fatalf("resolveSyncBranch() = %q, want debug-branch", got)
	}
}

// A remote that answers but whose HEAD names no branch is the genuine
// "unavailable": the error names the override, and nothing is recorded.
func TestResolveSyncBranchErrorsWhenRemoteAdvertisesNoDefault(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "")
	ws := pushedRepoWithoutRemoteHead(t)
	runGit(t, ws.RootDir, "--git-dir", remoteURL(t, ws), "symbolic-ref", "HEAD", "refs/heads/no-such-branch")
	_, err := resolveSyncBranch(context.Background(), ws, "origin")
	if err == nil {
		t.Fatal("expected error when the remote advertises no default branch")
	}
	if !strings.Contains(err.Error(), debugSyncBranchEnvVar) {
		t.Fatalf("error = %q, want mention of %s", err.Error(), debugSyncBranchEnvVar)
	}
	if _, err := os.Stat(advertisedSyncBranchPath(ws, "origin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record stat error = %v, want not-exist: an unavailable branch must not be recorded", err)
	}
}

// A failed ask surfaces its cause; a cancelled ctx is one, and it must not read
// as "default branch unavailable". [LAW:no-silent-failure]
func TestResolveSyncBranchSurfacesCancellationNotMisleadingUnavailable(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ws := branchTestWorkspace(t, t.TempDir())
	_, err := resolveSyncBranch(ctx, ws, "origin")
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled in the chain", err)
	}
	if strings.Contains(err.Error(), "default branch unavailable") {
		t.Fatalf("cancelled ctx must not surface the misleading unavailable message: %q", err.Error())
	}
}

// The read banner's branch is the one push and pull use: once a sync verb has
// asked the remote, a read resolves the same branch with no network.
func TestKnownSyncBranchReportsWhatTheSyncVerbResolved(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "")
	ws := pushedRepoWithoutRemoteHead(t)
	if _, err := knownSyncBranch(context.Background(), ws, "origin"); err == nil {
		t.Fatal("knownSyncBranch() before any sync: want the not-known error")
	}
	synced, err := resolveSyncBranch(context.Background(), ws, "origin")
	if err != nil {
		t.Fatalf("resolveSyncBranch() error = %v", err)
	}
	known, err := knownSyncBranch(context.Background(), ws, "origin")
	if err != nil {
		t.Fatalf("knownSyncBranch() after sync error = %v", err)
	}
	if known != "trunk" || known != synced {
		t.Fatalf("knownSyncBranch() = %q, resolveSyncBranch() = %q, want both trunk", known, synced)
	}
}

// The ticket's repro: refs/remotes/origin/HEAD unset and a remote that accepts
// and never answers. A read command's branch lookup returns at once instead of
// waiting on it. links-scale-om3r.06l
func TestKnownSyncBranchNeverAsksTheRemote(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "remote", "add", "origin", "git://"+listener.Addr().String()+"/x.git")
	ws := branchTestWorkspace(t, repo)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = knownSyncBranch(ctx, ws, "origin")
	if err == nil || !strings.Contains(err.Error(), "not known on this machine") {
		t.Fatalf("knownSyncBranch() error = %v, want the not-known error", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("knownSyncBranch() ran until the deadline: it waited on the remote")
	}
}

// pushedRepoWithoutRemoteHead is a repository whose origin was added and pushed
// to but never cloned from, so git has no refs/remotes/origin/HEAD; the bare
// remote's HEAD names trunk.
func pushedRepoWithoutRemoteHead(t *testing.T) workspace.Info {
	t.Helper()
	repo := t.TempDir()
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, repo, "init")
	runGit(t, repo, "checkout", "-b", "trunk")
	runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	runGit(t, repo, "init", "--bare", remote)
	runGit(t, repo, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "trunk")
	return branchTestWorkspace(t, repo)
}

func branchTestWorkspace(t *testing.T, root string) workspace.Info {
	t.Helper()
	return workspace.Info{RootDir: root, Location: workspace.Location{StorageDir: t.TempDir()}}
}

func remoteURL(t *testing.T, ws workspace.Info) string {
	t.Helper()
	url, err := workspace.GitRemotes(context.Background(), ws.RootDir)
	if err != nil || len(url) != 1 {
		t.Fatalf("GitRemotes() = %v, %v; want one remote", url, err)
	}
	return url[0].URL
}
