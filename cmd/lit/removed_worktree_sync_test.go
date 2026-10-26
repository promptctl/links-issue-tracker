//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// removedWorktreeRepo is a primary checkout with two git remotes, whose branch
// tracks origin, and a linked worktree whose branch tracks origin too. Two
// remotes make the sync-remote precedence ask the upstream, the one answer a
// checkout holds that the repository does not, so a worker that still asks its
// launching checkout fails here once that checkout is gone.
type removedWorktreeRepo struct {
	self, root, worktree, epic string
	storageDir                 string
	xdgConfigHome              string
	observer                   remoteObserver
}

func newRemovedWorktreeRepo(t *testing.T) removedWorktreeRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}
	base := t.TempDir()
	r := removedWorktreeRepo{self: self, root: filepath.Join(base, "workspace"), worktree: filepath.Join(base, "wt"), xdgConfigHome: filepath.Join(base, "xdg-config")}
	for _, dir := range []string{r.root, r.xdgConfigHome} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	runGit(t, r.root, "init")
	runGit(t, r.root, "config", "user.email", "wt@test.co")
	runGit(t, r.root, "config", "user.name", "wt")
	if err := os.WriteFile(filepath.Join(r.root, "readme.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, r.root, "add", "-A")
	runGit(t, r.root, "commit", "-m", "seed")
	branch := gitCurrentBranch(t, r.root)
	for _, remote := range []string{"origin", "backup"} {
		runGit(t, base, "init", "--bare", remote+".git")
		runGit(t, r.root, "remote", "add", remote, filepath.Join(base, remote+".git"))
		runGit(t, r.root, "push", remote, branch)
	}
	runGit(t, r.root, "branch", "--set-upstream-to", "origin/"+branch)

	quiet := isolatedEnv(r.xdgConfigHome, "1")
	if out, err := runLit(t, r.root, self, quiet, "init", "--skip-hooks"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out)
	}
	r.epic = newEpicViaLit(t, r.root, self, quiet, "worktree-epic")
	if out, err := runLit(t, r.root, self, quiet, "sync", "push", "--set-upstream"); err != nil {
		t.Fatalf("bootstrap lit sync push: %v\noutput:\n%s", err, out)
	}
	r.observer = newRemoteObserver(t, base, filepath.Join(base, "origin.git"), self, r.xdgConfigHome)

	runGit(t, r.root, "worktree", "add", "-b", "wt", r.worktree, "origin/"+branch)
	loc, err := workspace.Locate(testEnv(), r.root)
	if err != nil {
		t.Fatalf("locate the store: %v", err)
	}
	r.storageDir = loc.StorageDir
	return r
}

// litIn runs lit in dir and fails the test on a non-zero exit.
func (r removedWorktreeRepo) litIn(t *testing.T, dir string, env map[string]string, args ...string) string {
	t.Helper()
	out, err := runLit(t, dir, r.self, env, args...)
	if err != nil {
		t.Fatalf("lit %s in %s: %v\noutput:\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

// removeWorktreeThenJoin is the reported session's last act, `git worktree
// remove --force` straight after its write, followed by the wait for every
// worker that write spawned.
func (r removedWorktreeRepo) removeWorktreeThenJoin(t *testing.T) {
	t.Helper()
	runGit(t, r.root, "worktree", "remove", "--force", r.worktree)
	if err := cli.JoinDetachedWorkers(r.storageDir, workerJoinPatience); err != nil {
		t.Fatalf("%v\n%s\nreceive log:\n%s", err, dumpMirrorLog(r.storageDir), receiveLog(t, workspace.Info{Location: workspace.LocationFromStorageDir(r.storageDir)}))
	}
}

// requirePushed reads the last push attempt's outcome, the record the FAILING
// banner prints from, and requires that it landed.
func (r removedWorktreeRepo) requirePushed(t *testing.T) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(r.storageDir, "push-outcome.last"))
	if err != nil {
		t.Fatalf("read push outcome: %v", err)
	}
	var outcome struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(body, &outcome); err != nil {
		t.Fatalf("parse push outcome %q: %v", body, err)
	}
	if outcome.Decision != "pushed" {
		t.Fatalf("last push outcome is %q (%s), want pushed\n%s", outcome.Decision, outcome.Reason, dumpMirrorLog(r.storageDir))
	}
	if out := r.litIn(t, r.root, isolatedEnv(r.xdgConfigHome, "0"), "new", "--title", "after-removal", "--topic", "demo", "--parent", r.epic); strings.Contains(out, "FAILING") {
		t.Fatalf("the primary checkout prints the FAILING banner after the worktree's push landed:\n%s", out)
	}
}

func (r removedWorktreeRepo) requireOnRemote(t *testing.T, title string) {
	t.Helper()
	if n, err := r.observer.count(r.epic, title); err != nil || n != 1 {
		t.Fatalf("remote carries %d of %q (err %v), want 1\n%s", n, title, err, dumpMirrorLog(r.storageDir))
	}
}

// TestAWriteInARemovedWorktreeReachesTheRemote is links-scale-dy46.37o's report:
// a session writes in a linked worktree under the on-change cadence and removes
// the worktree seconds later, before its detached mirror reads anything. The
// mirror pushes anyway, from values its launcher handed it and questions it
// asks of the repository.
func TestAWriteInARemovedWorktreeReachesTheRemote(t *testing.T) {
	t.Parallel()
	r := newRemovedWorktreeRepo(t)

	r.litIn(t, r.worktree, isolatedEnv(r.xdgConfigHome, "0"), "new", "--title", "written-in-worktree", "--topic", "demo", "--parent", r.epic)
	r.removeWorktreeThenJoin(t)

	r.requireOnRemote(t, "written-in-worktree")
	r.requirePushed(t)
}

// TestAReceiveFromARemovedWorktreeMergesAndItsMirrorPushes is the same report
// for the background receive, and for the mirror a receive spawns once it has
// merged: that mirror's launcher is the receive worker, whose launching
// checkout is already gone. The worktree holds a write the remote lacks while
// a peer pushes one the worktree lacks, so the write's mirror is refused, the
// receive merges, and only the receive's mirror can land the merge.
func TestAReceiveFromARemovedWorktreeMergesAndItsMirrorPushes(t *testing.T) {
	t.Parallel()
	r := newRemovedWorktreeRepo(t)
	quiet := isolatedEnv(r.xdgConfigHome, "1")

	r.litIn(t, r.worktree, quiet, "new", "--title", "unpushed-in-worktree", "--topic", "demo", "--parent", r.epic)
	r.litIn(t, r.observer.dir, quiet, "new", "--title", "from-peer", "--topic", "demo", "--parent", r.epic)
	r.litIn(t, r.observer.dir, quiet, "sync", "push")

	r.litIn(t, r.worktree, isolatedEnv(r.xdgConfigHome, "0"), "new", "--title", "written-in-worktree", "--topic", "demo", "--parent", r.epic)
	r.removeWorktreeThenJoin(t)

	if ls := r.litIn(t, r.root, quiet, "ls", "--parent", r.epic, "--columns", "title"); !strings.Contains(ls, "from-peer") {
		t.Fatalf("the store lacks the peer's ticket after the receive worker ended:\n%s\nreceive log:\n%s", ls, receiveLog(t, workspace.Info{Location: workspace.LocationFromStorageDir(r.storageDir)}))
	}
	r.requireOnRemote(t, "unpushed-in-worktree")
	r.requireOnRemote(t, "written-in-worktree")
	r.requirePushed(t)
}
