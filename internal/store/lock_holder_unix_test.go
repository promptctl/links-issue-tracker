//go:build unix

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/deadline"
	"github.com/promptctl/links-issue-tracker/internal/suiteenv"
)

// A listing of the holder records that fails shows no holder standing, so a
// waiter stops judging by the terms of a holder that may have gone: behind a
// lit holder under long deadlines whose records cannot be listed (its arrival
// still can), the waiter falls back to the allowance for an unstated holder
// and reports why. The listing breaks at a step of the waiter's own clock.
func TestAFailedListingKeepsNoHoldersTerms(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root lists what a directory without read permission holds")
	}
	const unstated = 300 * time.Millisecond
	var waits []LockWait
	ctx := WithLockObserver(context.Background(), LockObserver{
		Waited:   func(w LockWait) { waits = append(waits, w) },
		Held:     func(LockHold) {},
		Recorded: func(LockRecord) {},
	})
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	holder, err := acquireStoreLock(context.Background(), testStderr(), storageDirOf(lockPath), lockPath, true, heldUnder(suiteenv.WithHold(time.Minute)), refuseHeld)
	if err != nil {
		t.Fatalf("holder acquireStoreLock() error = %v", err)
	}
	dir := lockHolderDir(storageDirOf(lockPath), lockPath)
	defer func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Errorf("restore the holder dir: %v", err)
		}
		if err := holder(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	}()

	// Search and write, no read: the arrival opens by name, the listing fails.
	clock := newStepClock(t, func(at time.Duration) {
		if at == deadline.LockPollInterval {
			if err := os.Chmod(dir, 0o300); err != nil {
				t.Fatalf("make the holder records unlistable: %v", err)
			}
		}
	})
	_, err = acquireStoreLockOn(ctx, testStderr(), operatorNotices(), clock, storageDirOf(lockPath), lockPath, true, suiteHolder(), judgedByHolder(unstated))
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("contended acquireStoreLockOn() error = %v, want ErrWorkspaceBusy", err)
	}
	if got := clock.elapsed(); got != unstated {
		t.Fatalf("the waiter gave up %s in, want %s: with no holder known to stand, the unstated allowance", got, unstated)
	}
	if len(waits) != 1 || waits[0].StandStill != unstated || waits[0].Stated || !errors.Is(waits[0].ListErr, os.ErrPermission) {
		t.Fatalf("lock waits reported = %+v, want one judged by the unstated %s, naming the failed listing", waits, unstated)
	}
}
