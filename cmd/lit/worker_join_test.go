//go:build !windows

package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

const (
	// workerJoinPatience bounds the join. A worker still running at cleanup can
	// have a whole designed life ahead of it — waiting out its parent, then a
	// mirror, then a deadline-bounded push or receive — each slow on a loaded
	// CI machine under -race. Generous headroom costs nothing when healthy,
	// because the wait ends the instant the last worker is gone.
	workerJoinPatience     = 2 * time.Minute
	workerJoinPollInterval = 20 * time.Millisecond
)

// joinedWorkspaces holds one entry per (test, directory) whose join runLit
// has registered, so a test that runs lit fifty times registers one cleanup.
// [LAW:no-shared-mutable-globals] written only by joinDetachedWorkers.
var joinedWorkspaces sync.Map

type workerJoinKey struct {
	t   *testing.T
	dir string
}

// joinDetachedWorkers makes t's TempDir sweep wait for every detached worker
// lit spawned for the store under dir. runLit calls it for every lit run, so
// no test that runs lit can forget it. A detached worker outlives the command
// that spawned it, and it writes under <root>/.git/links while t.TempDir's
// cleanup is removing that directory; an entry created between RemoveAll's
// readdir and its rmdir fails the sweep with "directory not empty".
//
// The join waits for each worker's process to be GONE, read from the kernel,
// never for a worker to look finished: a mirror drops its liveness beacon and
// a receive writes its end line before each one's last writes.
// [LAW:no-ambient-temporal-coupling] The pids come from the spawn records
// (cli.SpawnedWorkerPIDs), which the spawner writes before it exits, so a
// worker that has not run an instruction yet is already listed. A worker can
// spawn another, so the join ends only when every listed pid is dead AND a
// re-read finds no pid the dead ones left behind.
func joinDetachedWorkers(t *testing.T, dir string) {
	t.Helper()
	if _, registered := joinedWorkspaces.LoadOrStore(workerJoinKey{t, dir}, struct{}{}); registered {
		return
	}
	t.Cleanup(func() {
		joinedWorkspaces.Delete(workerJoinKey{t, dir})
		// Read at cleanup: the store may be created by a later lit run than
		// the one that registered this join. Discover only looks; it creates
		// nothing, so a directory lit never initialized stays untouched.
		locations, err := workspace.Discover([]string{dir})
		if err != nil {
			t.Errorf("worker-join cleanup: find the store under %s: %v", dir, err)
			return
		}
		for _, loc := range locations {
			awaitWorkersGone(t, loc.StorageDir)
		}
	})
}

func awaitWorkersGone(t *testing.T, storageDir string) {
	t.Helper()
	deadline := time.Now().Add(workerJoinPatience)
	for {
		listed, err := cli.SpawnedWorkerPIDs(storageDir)
		if err != nil {
			t.Errorf("worker-join cleanup: %v", err)
			return
		}
		live := slices.DeleteFunc(slices.Clone(listed), func(pid int) bool { return !processAlive(pid) })
		if len(live) == 0 {
			// Every listed worker is dead, so every record it could write is
			// already in the log; a pid this read did not list is a worker
			// one of them spawned.
			relisted, err := cli.SpawnedWorkerPIDs(storageDir)
			if err != nil {
				t.Errorf("worker-join cleanup: %v", err)
				return
			}
			if !slices.ContainsFunc(relisted, func(pid int) bool { return !slices.Contains(listed, pid) }) {
				return
			}
		}
		if time.Now().After(deadline) {
			// Loud, because the sweep that runs next is now known to race a
			// live worker: the RemoveAll error it produces would name the
			// symptom, and this names the cause. [LAW:no-silent-failure]
			t.Errorf("detached workers %v still running %s after cleanup began; the TempDir sweep will race them\n%s",
				live, workerJoinPatience, dumpWorkerLogs(storageDir))
			return
		}
		time.Sleep(workerJoinPollInterval)
	}
}

// dumpWorkerLogs renders every worker log under storageDir for a failure
// message.
func dumpWorkerLogs(storageDir string) string {
	var b strings.Builder
	for _, path := range cli.WorkerLogPaths(storageDir) {
		content, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(&b, "%s unavailable: %v\n", path, err)
			continue
		}
		fmt.Fprintf(&b, "%s:\n%s\n", path, content)
	}
	return b.String()
}
