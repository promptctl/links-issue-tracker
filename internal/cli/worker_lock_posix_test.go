//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/process"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
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
	useRealWorkers(t)
	maskGlobalConfig(t)
	rig := setupMirrorDeadlineRepo(t)

	// The notifier outlives the join's patience below, so a hold it inherited
	// would keep the join waiting past it.
	notifierPIDFile := filepath.Join(rig.base, "notifier.pid")
	notifierLife := int((2 * suiteStallDeadline).Seconds())
	writeOwnerNotifyProjectConfig(t, rig.root, "[sync]\ncadence = 'on-push'\nreceive = false\n"+
		ownerHookKey(fmt.Sprintf(`sleep %d >/dev/null 2>&1 & echo $! > %s`, notifierLife, shPath(notifierPIDFile))))
	// The rig's unpushed commit goes nowhere, so the mirror's push fails and
	// runs the hook.
	runGit(t, rig.root, "remote", "set-url", "origin", filepath.Join(rig.base, "gone", "nowhere.git"))

	ws := workspacetest.Resolved(t, runEnv(t), rig.root)
	notifier := func() (int, error) {
		pidText, err := os.ReadFile(notifierPIDFile)
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(strings.TrimSpace(string(pidText)))
	}
	// Whatever the verdict, the notifier the hook started goes with the test.
	t.Cleanup(func() {
		pid, err := notifier()
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return // the hook never ran, so there is no notifier to end
		case err != nil:
			t.Errorf("read the notifier's pid to end it: %v", err)
		default:
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Errorf("end the notifier (pid %d): %v", pid, err)
			}
		}
	})
	// The spawner is this test process, which stays up, so the mirror is
	// handed none to wait for. Its owner channel is on: the mirror's run is
	// the one this test enables automatic sync for.
	mirror := workerRun(t, testStderr(t))
	mirror = mirror.withEnv(mirror.Env.With(DisableAutoSyncEnvVar + "=0"))
	if err := spawnBackgroundMirror(underInvocation(t.Context()), mirror, ws, noSpawner{}); err != nil {
		t.Fatalf("spawnBackgroundMirror: %v", err)
	}
	if err := JoinDetachedWorkers(ws.StorageDir, suiteStallDeadline); err != nil {
		t.Fatalf("JoinDetachedWorkers() with only the mirror's notifier alive = %v; want nil\n%s", err, workerLog(ws, mirrorLogName))
	}
	notifierPID, err := notifier()
	if err != nil {
		t.Fatalf("the mirror never ran the hook that backgrounds a notifier (%v), so the join proved nothing\n%s", err, workerLog(ws, mirrorLogName))
	}
	if !process.Alive(notifierPID) {
		t.Fatalf("the notifier (pid %d) was gone when the join returned, so the join proved nothing about a child outliving its worker", notifierPID)
	}
}

// workerLog is a worker's log in ws, headed by its name, or why it could not
// be read, for a failure message.
func workerLog(ws workspace.Info, name string) string {
	log, err := os.ReadFile(filepath.Join(ws.StorageDir, name))
	if err != nil {
		return fmt.Sprintf("%s unreadable: %v", name, err)
	}
	return name + ":\n" + string(log)
}
