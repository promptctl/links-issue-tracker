//go:build !windows

package cli

import (
	"strings"
	"testing"
	"time"
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
