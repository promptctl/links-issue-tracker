//go:build !windows

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestJoinDetachedWorkersWaitsForEveryHoldToBeReleased pins the join's meaning:
// while any spawn's shared hold is open the join does not return, and once
// the last one is released it does.
func TestJoinDetachedWorkersWaitsForEveryHoldToBeReleased(t *testing.T) {
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
	if err := JoinDetachedWorkers(t.TempDir(), time.Minute); err != nil {
		t.Fatalf("JoinDetachedWorkers(store with no lock) = %v; want nil", err)
	}
}

// workerOutlivedByChildEnv makes this test binary run as a detached worker
// whose child outlives it (runWorkerOutlivedByChild).
const workerOutlivedByChildEnv = "LIT_TEST_WORKER_OUTLIVED_BY_CHILD"

// runWorkerOutlivedByChild is a worker that starts a child it never waits for,
// prints the child's pid and exits — the shape of a backgrounded owner-notify
// hook.
func runWorkerOutlivedByChild() {
	leaf := detachedWorkerLeaf("worker-outlived-by-child", "", func(context.Context, io.Writer, workspace.Info, int) error {
		child := exec.Command("sleep", "60")
		if err := child.Start(); err != nil {
			return err
		}
		fmt.Println(child.Process.Pid)
		return nil
	})
	if err := leaf.work(context.Background(), os.Stdout, workspace.Info{}, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestJoinDetachedWorkersEndsWithTheWorkerNotItsChildren pins that a worker's
// hold is its own: a child the worker started and left running does not keep
// the join waiting once the worker has exited.
func TestJoinDetachedWorkersEndsWithTheWorkerNotItsChildren(t *testing.T) {
	storageDir := t.TempDir()
	holds, err := holdWorkerLock(storageDir)
	if err != nil {
		t.Fatalf("holdWorkerLock() = %v", err)
	}
	worker := exec.Command(os.Args[0])
	worker.Env = append(os.Environ(), workerOutlivedByChildEnv+"=1")
	worker.ExtraFiles = holds
	out, err := worker.Output()
	_ = holds[0].Close()
	if err != nil {
		t.Fatalf("worker: %v\n%s", err, out)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("worker printed %q, want the child's pid", out)
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	if err := JoinDetachedWorkers(storageDir, 5*time.Second); err != nil {
		t.Fatalf("JoinDetachedWorkers() with only the worker's child alive = %v; want nil", err)
	}
}
