package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func TestResolveSyncBranchUsesDebugOverrideWhenPresent(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "debug-branch")
	ws := pushedRepoWithoutRemoteHead(t)
	got, err := resolveSyncBranch(underInvocation(context.Background()), testEnv(), ws, originRemote(t, ws))
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
	runGit(t, ws.RootDir, "--git-dir", originRemote(t, ws).URL(), "symbolic-ref", "HEAD", "refs/heads/no-such-branch")
	_, err := resolveSyncBranch(underInvocation(context.Background()), testEnv(), ws, originRemote(t, ws))
	if err == nil {
		t.Fatal("expected error when the remote advertises no default branch")
	}
	if !strings.Contains(err.Error(), debugSyncBranchEnvVar) {
		t.Fatalf("error = %q, want mention of %s", err.Error(), debugSyncBranchEnvVar)
	}
	if _, err := os.Stat(advertisedSyncBranchPath(ws, remoteName(t, "origin"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record stat error = %v, want not-exist: an unavailable branch must not be recorded", err)
	}
}

// A failed ask surfaces its cause; a cancelled ctx is one, and it must not read
// as "default branch unavailable". [LAW:no-silent-failure]
func TestResolveSyncBranchSurfacesCancellationNotMisleadingUnavailable(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "")
	ctx, cancel := context.WithCancel(underInvocation(context.Background()))
	cancel()
	ws := pushedRepoWithoutRemoteHead(t)
	_, err := resolveSyncBranch(ctx, testEnv(), ws, originRemote(t, ws))
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
	ctx := underInvocation(context.Background())
	if _, err := knownSyncBranch(ctx, testEnv(), ws, remoteName(t, "origin")); err == nil {
		t.Fatal("knownSyncBranch() before any sync: want the not-known error")
	}
	synced, err := resolveSyncBranch(ctx, testEnv(), ws, originRemote(t, ws))
	if err != nil {
		t.Fatalf("resolveSyncBranch() error = %v", err)
	}
	known, err := knownSyncBranch(ctx, testEnv(), ws, remoteName(t, "origin"))
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
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "remote", "add", "origin", silentRemoteURL(t))
	ws := branchTestWorkspace(t, repo)

	ctx, cancel := context.WithTimeout(underInvocation(context.Background()), 5*time.Second)
	defer cancel()
	_, err := knownSyncBranch(ctx, testEnv(), ws, remoteName(t, "origin"))
	if err == nil || !strings.Contains(err.Error(), "not known on this machine") || strings.Contains(err.Error(), "%!") {
		t.Fatalf("knownSyncBranch() error = %v, want the not-known error", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("knownSyncBranch() ran until the deadline: it waited on the remote")
	}
}

// A cancel that lands while the ask is on the wire is the cause the error names,
// not the "signal: killed" the interrupted git exits with. [LAW:no-silent-failure]
func TestResolveSyncBranchSurfacesCancellationMidAsk(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "")
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "remote", "add", "origin", silentRemoteURL(t))
	ws := branchTestWorkspace(t, repo)
	remote := originRemote(t, ws)
	ctx, cancel := context.WithTimeout(underInvocation(context.Background()), 300*time.Millisecond)
	defer cancel()
	_, err := resolveSyncBranch(ctx, testEnv(), ws, remote)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded in the chain", err)
	}
}

// silentRemoteURL is a git:// remote that accepts every connection and never
// answers, so an ask of it runs until its ctx ends.
func silentRemoteURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		var held []net.Conn // held open, never answered, until the listener closes
		for {
			conn, err := listener.Accept()
			if err != nil {
				for _, c := range held {
					_ = c.Close()
				}
				return
			}
			held = append(held, conn)
		}
	}()
	return "git://" + listener.Addr().String() + "/x.git"
}

// An unreadable record cannot hide a branch git already knows: it is consulted
// only after the local chain.
func TestKnownSyncBranchPrefersGitsRefOverAnUnreadableRecord(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "")
	ws := pushedRepoWithoutRemoteHead(t)
	runGit(t, ws.RootDir, "remote", "set-head", "origin", "trunk")
	if err := os.Mkdir(advertisedSyncBranchPath(ws, remoteName(t, "origin")), 0o755); err != nil {
		t.Fatalf("make the record unreadable: %v", err)
	}
	got, err := knownSyncBranch(underInvocation(context.Background()), testEnv(), ws, remoteName(t, "origin"))
	if err != nil || got != "trunk" {
		t.Fatalf("knownSyncBranch() = %q, %v; want trunk", got, err)
	}
	runGit(t, ws.RootDir, "remote", "set-head", "origin", "--delete")
	_, err = knownSyncBranch(underInvocation(context.Background()), testEnv(), ws, remoteName(t, "origin"))
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("knownSyncBranch() error = %v, want the unreadable record named", err)
	}
	if errors.As(err, new(syncBranchNotKnownError)) {
		t.Fatalf("knownSyncBranch() error = %v is a syncBranchNotKnownError; an unreadable record is a fault, not the absence a caller skips on", err)
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
	checkout := checkoutRoot(t, root)
	return workspace.Info{
		RootDir:  checkout.String(),
		Location: workspace.Location{GitCommonDir: filepath.Join(checkout.String(), ".git"), StorageDir: t.TempDir()},
		Remotes:  workspace.NewRemotes(testEnv(), checkout),
	}
}

// originRemote is the workspace's one remote, as lit syncs with it.
func originRemote(t *testing.T, ws workspace.Info) workspace.SyncableRemote {
	t.Helper()
	remotes, err := workspace.NewRemotes(testEnv(), checkoutRoot(t, ws.RootDir)).List(underInvocation(context.Background()))
	if err != nil || len(remotes) != 1 {
		t.Fatalf("Remotes.List() = %v, %v; want one remote", remotes, err)
	}
	remote, err := remotes[0].SyncableRemote()
	if err != nil {
		t.Fatalf("SyncableRemote() error = %v", err)
	}
	return remote
}

// The event names which answer decided the branch: the override, git's
// remote HEAD, the remote's advertised HEAD, or the record of it a read uses.
// [LAW:nothing-unseen]
func TestSyncBranchSourceIsOnTheEvent(t *testing.T) {
	source := func(t *testing.T, resolve func(ctx context.Context) (string, error)) string {
		t.Helper()
		rec := telemetry.Begin(telemetry.Event{})
		if _, err := resolve(telemetry.With(context.Background(), rec)); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return fmt.Sprint(rec.Finish(telemetry.End{}).Facts["sync.branch_source"])
	}
	ws := pushedRepoWithoutRemoteHead(t)
	resolve := func(ctx context.Context) (string, error) {
		return resolveSyncBranch(ctx, testEnv(), ws, originRemote(t, ws))
	}
	known := func(ctx context.Context) (string, error) {
		return knownSyncBranch(ctx, testEnv(), ws, remoteName(t, "origin"))
	}

	t.Setenv(debugSyncBranchEnvVar, "")
	if got := source(t, resolve); got != "advertised" {
		t.Errorf("unset remote HEAD: sync.branch_source = %q, want advertised", got)
	}
	if got := source(t, known); got != "recorded" {
		t.Errorf("read after a sync: sync.branch_source = %q, want recorded", got)
	}
	runGit(t, ws.RootDir, "remote", "set-head", "origin", "trunk")
	if got := source(t, resolve); got != "remote_head" {
		t.Errorf("set remote HEAD: sync.branch_source = %q, want remote_head", got)
	}
	t.Setenv(debugSyncBranchEnvVar, "debug-branch")
	if got := source(t, known); got != "override" {
		t.Errorf("override: sync.branch_source = %q, want override", got)
	}
}

// A git that cannot say which remote the branch tracks fails the selection
// that needs the answer, loudly, and no selection that names its remote. The
// second remote is what makes the upstream decide.
//
// Not parallel: it puts a git on PATH that fails `for-each-ref`.
func TestAnUnreadableUpstreamFailsOnlyTheSelectionThatReadsIt(t *testing.T) {
	repo := pushedRepoWithoutRemoteHead(t)
	runGit(t, repo.RootDir, "remote", "add", "fork", filepath.Join(t.TempDir(), "fork.git"))
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	shim := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = for-each-ref ] && { echo 'fatal: ref lock' >&2; exit 128; }; done\nexec " + gitPath + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ws := branchTestWorkspace(t, repo.RootDir)

	if remote, ok, err := workspaceSyncRemote(underInvocation(context.Background()), ws, "origin"); err != nil || !ok || remote.Name().String() != "origin" {
		t.Fatalf("workspaceSyncRemote(origin) = %q, %v, %v; want origin, needing no upstream", remote.Name(), ok, err)
	}
	if remote, ok, err := workspaceSyncRemote(underInvocation(context.Background()), ws, ""); err == nil || !strings.Contains(err.Error(), "ref lock") {
		t.Fatalf("workspaceSyncRemote(\"\") = %q, %v, %v; want the upstream read's failure, not a fallback to none", remote.Name(), ok, err)
	}
}
