//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/workspace/workspacetest"
)

// TestJoinDetachedWorkersWaitsForEveryHoldToBeReleased pins the join's meaning:
// while any spawn's shared hold is open the join does not return, and once
// the last one is released it does.
func TestJoinDetachedWorkersWaitsForEveryHoldToBeReleased(t *testing.T) {
	t.Parallel()
	storageDir := t.TempDir()
	holds, err := holdWorkerLock(storageDir)
	if err != nil {
		t.Fatalf("holdWorkerLock() = %v", err)
	}

	err = JoinDetachedWorkers(storageDir, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), workerLockName) {
		t.Fatalf("JoinDetachedWorkers() with a hold open = %v; want an error naming %s", err, workerLockName)
	}

	if err := holds[0].Close(); err != nil {
		t.Fatalf("release the hold: %v", err)
	}
	if err := JoinDetachedWorkers(storageDir, 100*time.Millisecond); err != nil {
		t.Fatalf("JoinDetachedWorkers() with every hold released = %v; want nil", err)
	}
}

// TestJoinDetachedWorkersOfAStoreNoWorkerWasSpawnedForReturns pins that a store
// with no worker lock answers at once: no spawn ever took a hold there.
func TestJoinDetachedWorkersOfAStoreNoWorkerWasSpawnedForReturns(t *testing.T) {
	t.Parallel()
	if err := JoinDetachedWorkers(t.TempDir(), time.Minute); err != nil {
		t.Fatalf("JoinDetachedWorkers(store with no lock) = %v; want nil", err)
	}
}

// TestJoinDetachedWorkersEndsWithTheWorkerNotItsChildren pins that a worker's
// hold is its own, in the production shape that needs it: a mirror whose push
// fails runs the owner-notify hook, the hook backgrounds a notifier and
// returns, and the mirror exits. The join then ends with the mirror, though
// the notifier still runs.
func TestJoinDetachedWorkersEndsWithTheWorkerNotItsChildren(t *testing.T) {
	t.Parallel()
	setRunEnv(t, DisableAutoSyncEnvVar, "1")
	maskGlobalConfig(t)
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	repo := filepath.Join(base, "solo")
	runGit(t, base, "clone", filepath.Join(base, "remote.git"), "solo")
	runGit(t, repo, "config", "user.email", "s@s.co")
	runGit(t, repo, "config", "user.name", "solo")
	if err := os.WriteFile(filepath.Join(repo, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed")
	runGit(t, repo, "push", "origin", "HEAD")
	runCLIInDir(t, repo, "init", "--skip-hooks")
	newEpicInDir(t, repo, "pushed-epic")
	runCLIInDir(t, repo, "sync", "push", "--set-upstream")

	// The notifier outlives the join's patience below, so a hold it inherited
	// would keep the join waiting past it.
	notifierPIDFile := filepath.Join(base, "notifier.pid")
	writeOwnerNotifyProjectConfig(t, repo, "[sync]\ncadence = 'on-push'\nreceive = false\n"+
		ownerHookKey(`sleep 600 >/dev/null 2>&1 & echo $! > `+shPath(notifierPIDFile)))
	runGit(t, repo, "remote", "set-url", "origin", filepath.Join(base, "gone", "nowhere.git"))
	newEpicInDir(t, repo, "unpushed-epic")
	setRunEnv(t, DisableAutoSyncEnvVar, "0")

	ws := workspacetest.Resolved(t, runEnv(t), repo)
	mirrorLog := func() string {
		log, err := os.ReadFile(filepath.Join(ws.StorageDir, mirrorLogName))
		if err != nil {
			return err.Error()
		}
		return string(log)
	}
	notifier := func() (int, error) {
		pidText, err := os.ReadFile(notifierPIDFile)
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(strings.TrimSpace(string(pidText)))
	}
	// Whatever the verdict, the notifier the hook started goes with the test.
	t.Cleanup(func() {
		if pid, err := notifier(); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	// Parent pid 0: the spawner is this test process, which stays up.
	if err := spawnBackgroundMirror(underInvocation(t.Context()), testStderr(t), runEnv(t), ws, 0); err != nil {
		t.Fatalf("spawnBackgroundMirror: %v", err)
	}
	if err := JoinDetachedWorkers(ws.StorageDir, suiteStallDeadline); err != nil {
		t.Fatalf("JoinDetachedWorkers() with only the mirror's notifier alive = %v; want nil\n%s:\n%s", err, mirrorLogName, mirrorLog())
	}
	notifierPID, err := notifier()
	if err != nil {
		t.Fatalf("the mirror never ran the hook that backgrounds a notifier (%v), so the join proved nothing\n%s:\n%s", err, mirrorLogName, mirrorLog())
	}
	if err := syscall.Kill(notifierPID, 0); err != nil {
		t.Fatalf("the notifier (pid %d) was gone when the join returned (%v), so the join proved nothing about a child outliving its worker", notifierPID, err)
	}
}
