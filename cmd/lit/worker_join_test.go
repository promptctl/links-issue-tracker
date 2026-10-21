//go:build !windows

package main

import (
	"errors"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// workerJoinPatience bounds the join. A worker still running at cleanup can
// have a whole designed life ahead of it — waiting out its parent, then a
// mirror, then a deadline-bounded push or receive — each slow on a loaded CI
// machine under -race. Generous headroom costs nothing when healthy, because
// the wait ends the instant the last worker is gone.
const workerJoinPatience = 2 * time.Minute

// joinDetachedWorkers makes t's TempDir sweep wait for every detached worker a
// lit run in dir spawns. runLit calls it for every lit run, so no test that
// runs lit can forget it. A detached worker outlives the command that spawned
// it and writes under <root>/.git/links while t.TempDir's cleanup removes that
// directory; an entry created between RemoveAll's readdir and its rmdir fails
// the sweep with "directory not empty".
//
// The store is located the way lit locates it, upward from dir; a dir in no
// git repository has no store, so no lit run there spawns a worker.
func joinDetachedWorkers(t *testing.T, dir string) {
	t.Helper()
	loc, err := workspace.Locate(testEnv(), dir)
	if errors.Is(err, workspace.ErrNotGitRepo) {
		return
	}
	if err != nil {
		t.Fatalf("locate the store for %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := cli.JoinDetachedWorkers(loc.StorageDir, workerJoinPatience); err != nil {
			// Loud, because the sweep that runs next is now known to race a
			// live worker: the RemoveAll error it produces would name the
			// symptom, and this names the cause. [LAW:no-silent-failure]
			t.Errorf("%v; the TempDir sweep will race them\nreceive log:\n%s\n%s",
				err, receiveLog(t, workspace.Info{Location: loc}), dumpMirrorLog(loc.StorageDir))
		}
	})
}
