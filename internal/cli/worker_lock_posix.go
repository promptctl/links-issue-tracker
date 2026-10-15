//go:build !windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/promptctl/primitives/filelock"
)

// workerLockName is the lock under the storage dir that every detached worker
// holds shared for its whole life.
const workerLockName = ".links-detached-workers.lock"

const workerJoinPoll = 20 * time.Millisecond

func workerLockPath(storageDir string) string {
	return filepath.Join(storageDir, workerLockName)
}

// holdWorkerLock takes the shared hold a spawner hands to a detached worker as
// an inherited fd. An flock belongs to the open file description, which the
// child shares from the moment it exists, so the kernel releases the hold only
// once the worker is gone (keepWorkerHoldFromChildren keeps it from the
// worker's own children).
func holdWorkerLock(storageDir string) ([]*os.File, error) {
	file, err := os.OpenFile(workerLockPath(storageDir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the detached-worker lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH); err != nil {
		return nil, errors.Join(fmt.Errorf("hold the detached-worker lock: %w", err), file.Close())
	}
	return []*os.File{file}, nil
}

// workerHoldFD is where a worker finds its hold: os/exec numbers ExtraFiles
// from fd 3, and spawnDetachedWorker passes the hold as the only one.
const workerHoldFD = 3

// keepWorkerHoldFromChildren marks the inherited hold close-on-exec, which the
// dup into fd 3 cleared, so no process this worker starts inherits it.
func keepWorkerHoldFromChildren() { syscall.CloseOnExec(workerHoldFD) }

// JoinDetachedWorkers waits up to patience until no detached worker spawned for
// the store at storageDir is alive. The exclusive hold is granted only once
// every shared hold a spawn handed out is released, and the kernel releases one
// only at process exit, so the answer is read from the kernel, never from a
// worker that looks finished. A store no worker was ever spawned for has no
// lock and nothing to wait for.
func JoinDetachedWorkers(storageDir string, patience time.Duration) error {
	lockPath := workerLockPath(storageDir)
	if _, err := os.Stat(lockPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("find the detached-worker lock: %w", err)
	}
	// [LAW:dataflow-not-control-flow] patience bounds the wait, so the context
	// is the honest never-cancels value.
	release, acquired, err := filelock.Acquire(context.Background(), lockPath, true, int(patience/workerJoinPoll)+1, workerJoinPoll)
	if err != nil {
		return fmt.Errorf("join detached workers: %w", err)
	}
	if !acquired {
		return fmt.Errorf("detached workers still hold %s after %s", lockPath, patience)
	}
	return release()
}
