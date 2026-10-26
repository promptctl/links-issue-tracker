package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
)

// TestHoldForRefWriteRefusesUnderTheCommitLock pins the lock order where
// Dolt's LOCK is taken: every write that holds the store from outside its
// engine — a follow, the bound's re-root, a landing, a pushed-head record —
// is refused, before it opens anything, when its caller holds the commit lock.
func TestHoldForRefWriteRefusesUnderTheCommitLock(t *testing.T) {
	t.Parallel()
	root := migratedDoltDir(t)
	lockedCtx := context.WithValue(context.Background(), commitLockContextKey{}, time.Now())
	wrote := false
	err := holdForRefWrite(lockedCtx, root, engineWrite, time.Minute, func(context.Context, *doltdb.DoltDB) error {
		wrote = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "the commit lock is held") {
		t.Fatalf("holdForRefWrite() under the commit lock error = %v, want the lock-order refusal", err)
	}
	if wrote {
		t.Fatal("holdForRefWrite() ran its write under the commit lock")
	}
}
