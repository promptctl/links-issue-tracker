// Package filelock owns the kernel-enforced advisory file lock primitive
// (POSIX flock(2), Win32 LockFileEx) and the one acquisition loop every
// flock-backed coordination point runs. A hold is tied to the holding
// process's open file description, so the kernel releases it on ANY process
// death — SIGKILL included — which is what makes these locks usable as
// liveness evidence, not just mutual exclusion: "I acquired exclusively"
// proves every prior holder is gone, with no PID files, no staleness
// heuristics, and no timing bets.
//
// [LAW:one-source-of-truth] The OpenFile → tryLockFile → retry-or-error
// sequence lives here once; a lock's meaning lives entirely in which path and
// (exclusive, maxAttempts, delay) the caller passes. Extracted from
// internal/store's workspace lock so non-store packages (dbsnapshot's
// snapshot-producer beacon) share the same primitive instead of growing a
// second platform seam.
//
// [LAW:locality-or-seam] The platform variability lives behind the
// tryLockFile / unlockFile seam in filelock_posix.go and filelock_windows.go;
// adding a platform edits neither this file nor any caller.
package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// errWouldBlock is the internal seam between the platform-neutral acquisition
// loop and the platform-specific tryLockFile. The loop converts it into the
// acquired=false outcome after retries are exhausted; any other error from
// tryLockFile is a real failure surfaced immediately.
var errWouldBlock = errors.New("lock would block")

// Acquire takes a hold on the lock file at lockPath — shared (coexists with
// other shared holders) or exclusive (excludes everyone) — retrying up to
// maxAttempts with delay between attempts while the hold would block.
// maxAttempts of 1 is the non-blocking probe: it never sleeps.
//
// Three outcomes: (release, true, nil) — acquired, and the caller must run
// release (it unlocks and closes the FD; the kernel would also release on
// process death, which is the liveness property callers lean on);
// (nil, false, nil) — contention, every attempt would have blocked;
// (nil, false, err) — real failure (open error, lock syscall error, ctx
// cancellation, or an FD-close error while backing out).
//
// [LAW:types-are-the-program] Contention is a legal outcome of a healthy
// lock, not a failure, so it travels as a value — each caller stamps its own
// domain meaning (store's ErrWorkspaceBusy, a collector's silent skip) at its
// own boundary instead of all sharing one sentinel's identity and text.
func Acquire(ctx context.Context, lockPath string, exclusive bool, maxAttempts int, delay time.Duration) (func() error, bool, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, false, fmt.Errorf("ensure lock dir: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock file: %w", err)
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = tryLockFile(file, exclusive)
		if err == nil {
			fd := file
			// [LAW:no-silent-failure] Both unlock and close failures matter
			// (lock stuck held; FD leak) so the release contract surfaces
			// them jointly via errors.Join instead of picking one.
			return func() error {
				var unlockErr error
				if e := unlockFile(fd); e != nil {
					unlockErr = fmt.Errorf("release file lock: %w", e)
				}
				var closeErr error
				if e := fd.Close(); e != nil {
					closeErr = fmt.Errorf("close file lock fd: %w", e)
				}
				return errors.Join(unlockErr, closeErr)
			}, true, nil
		}
		if !errors.Is(err, errWouldBlock) {
			return nil, false, joinWithClose(fmt.Errorf("lock %s: %w", lockPath, err), file)
		}
		if attempt+1 == maxAttempts {
			break
		}
		if waitErr := sleepWithContext(ctx, delay); waitErr != nil {
			return nil, false, joinWithClose(waitErr, file)
		}
	}
	if closeErr := file.Close(); closeErr != nil {
		return nil, false, fmt.Errorf("close file lock fd: %w", closeErr)
	}
	return nil, false, nil
}

// joinWithClose closes the lock file and returns the primary error joined
// with any close error, so an FD leak stays observable alongside the failure
// that triggered the release. [LAW:no-silent-failure]
func joinWithClose(primary error, file *os.File) error {
	if closeErr := file.Close(); closeErr != nil {
		return errors.Join(primary, fmt.Errorf("close file lock fd: %w", closeErr))
	}
	return primary
}

// sleepWithContext waits out delay unless ctx is done first, returning the
// context's error so an interrupted acquisition surfaces as cancellation
// rather than as a spurious ErrBusy.
func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
