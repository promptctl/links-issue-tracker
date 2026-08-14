//go:build !windows

package main

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// awaitMirrorQuiescence makes a test's TempDir sweep safe against the detached
// on-change mirror, with no timing bet. [LAW:no-ambient-temporal-coupling]
//
// A test that enables auto-sync leaves its LAST spawned mirror running when the
// test body returns — detached by design, it may still be opening or closing
// its engine (writing under <root>/.git/links) exactly while t.TempDir's
// cleanup sweeps the directory, which fails RemoveAll with "directory not
// empty". The single-flight sync-push lock is the deterministic quiescence
// signal for both states a mirror can be in:
//
//   - an ACTIVE mirror holds the lock from before its engine opens until after
//     the engine has closed, so acquiring the lock here proves no engine write
//     can land in the directory anymore;
//   - a mirror that has NOT yet reached its lock attempt finds the lock held
//     and exits by the silent coalescing path — no store open, no trace write,
//     no file created.
//
// The lock is deliberately never released: releasing before the directory
// sweep would re-open the window for a late mirror, and the kernel drops the
// flock when the test process exits. Registered via t.Cleanup AFTER the
// t.TempDir call, so it runs BEFORE the directory removal (cleanups are LIFO).
func awaitMirrorQuiescence(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		ws, err := workspace.Resolve(root)
		if err != nil {
			t.Logf("mirror-quiescence cleanup: resolve workspace: %v", err)
			return
		}
		const patience = 15 * time.Second
		deadline := time.Now().Add(patience)
		for {
			_, acquired, err := store.TryAcquireSyncPushLock(ws.DatabasePath)
			if err != nil {
				t.Logf("mirror-quiescence cleanup: acquire sync-push lock: %v", err)
				return
			}
			if acquired {
				return
			}
			if time.Now().After(deadline) {
				t.Logf("a mirror still held the sync-push lock after %s; the TempDir sweep may race it", patience)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}
