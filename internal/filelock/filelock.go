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
//
// # The lock discipline
//
// This package doc is also the one home of lit's lock discipline — the rules
// every coordination point on a workspace's Dolt directory follows. They live
// here, beside the primitive they orbit, so an agent adding a coordination
// point finds one pattern to copy and one place to put the file.
// [LAW:one-source-of-truth]
//
// ONE PRIMITIVE. Owner exclusion — any point where a holder must exclude
// others across a window of time — is an flock through Acquire, and "is the
// owner still alive" is answered only by exclusive acquisition, never by an
// mtime, a PID probe, or a wall-clock threshold. The kernel's answer is right
// on every death mode; every heuristic has been measured evicting a live
// holder (a commit-lock file backdated eleven minutes let a second process's
// mutation walk past its running owner and exit 0). Each lock declares its
// retry budget at its own call site, and contention travels as Acquire's
// acquired=false value, given its domain meaning at each caller's own
// boundary — a store sentinel, a collector's deliberate silent skip, a
// mirror's coalesce. Name allocation is not owner exclusion — the trace
// file's O_EXCL retry claims a unique name and holds nothing, and the
// snapshot slot's os.Mkdir reservation, though held across the copy window,
// has its owner's liveness proven by the beacon it sits under — so neither
// carries a liveness question of its own and both stay off this primitive.
// One mechanism predates the rule and is being rebuilt onto it: the
// mirror-pending marker's age-out (links-locking-il18.4). Copy a compliant
// lock, not that.
//
// ONE ACQUISITION ORDER, outermost to innermost:
//
//	workspace → engine → Dolt's own .dolt/noms/LOCK → commit → beacon
//
// A holder of an inner lock never waits on an outer one. Two entries need
// spelling out, because no lit call site shows them:
//
// Dolt's LOCK is acquired by the embedded driver, not by lit: a write-capable
// engine takes it when it opens and holds it until it closes, and a Store's
// engine opens lazily at first SQL use, so a live write Store stands at
// "holds LOCK, takes commit per mutation" for its whole lifetime. That
// standing hold is the trap in the natural reading of "hold Dolt's LOCK
// during a walk": taking LOCK (opening a write engine, or locking the file
// directly) while holding the commit lock inverts the order against every
// live write Store and can deadlock. Take it before commit or not at all. A
// deviation exists today and is tolerated, not copied: Store.Open's
// long-lived engine acquires LOCK under the commit lock — that Store's
// first SQL use is migrate, inside withCommitLock (ensureDoltDatabase's
// transient bootstrap engines have already taken and released LOCK before
// that, and OpenSync's engine first runs SQL outside any commit lock,
// following the rule) — and a GC-contention retry that rotates the
// connection mid-mutation re-acquires LOCK there too. It cannot cycle only
// because a write engine that can open under the commit lock belongs to a
// Store whose open the engine lock serialized before commit was reached
// (the one write engine outside the Store lifecycle, the adopt clone's,
// runs under the exclusive workspace hold and never takes the commit
// lock), and read engines never wait on LOCK (they keep Dolt's read-only
// fallback). links-locking-il18.3, which retires the engine lock as a
// partial shadow of LOCK, owns keeping that argument true.
//
// The sync-push lock sits outside the slots, outermost in practice: its
// holder goes on to take everything else (the mirror cycle opens a full
// write Store), but every acquisition of it is a non-blocking probe
// (maxAttempts 1), so no process ever waits ON it — and a lock with no
// inbound wait-edge cannot complete a cycle.
//
// ONE HOME. A lock file sits beside the dolt directory — at
// dirname(databasePath), the position every *LockPath helper in
// internal/store mints — so a `lit snapshots restore` that rotates the dolt
// directory cannot move the lock out from under its acquirers. An exception
// states its reason at the site that mints the path; there are two: the
// snapshot producer beacon lives inside snapshots/ with the artifacts whose
// liveness it proves, and the adopt-pending marker (a condemnation record,
// not a lock) lives inside the dolt root precisely so the rotation the locks
// must survive carries the marker with the directory it describes.
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
	// [LAW:no-silent-failure] A non-positive budget would skip the loop and
	// read as clean contention on a lock nobody holds — a caller bug reported
	// as a healthy holder. Refuse it loudly instead.
	if maxAttempts < 1 {
		return nil, false, fmt.Errorf("filelock: maxAttempts must be >= 1, got %d", maxAttempts)
	}
	// A caller that has already been cancelled must not take — and then hold —
	// a lock: a SIGTERM'd command acquiring an exclusive hold on its way down
	// would admit its guarded operation (a destructive directory rotation, say)
	// into the interrupt grace window. Refuse before any attempt, so a done
	// context is an error on every path, free lock included.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, false, ctxErr
	}
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
		if waitErr := SleepWithContext(ctx, delay); waitErr != nil {
			return nil, false, joinWithClose(waitErr, file)
		}
	}
	if closeErr := file.Close(); closeErr != nil {
		return nil, false, fmt.Errorf("close file lock fd: %w", closeErr)
	}
	// [LAW:parse-dont-validate] Cancellation and contention are different
	// facts, and only the sleeps between attempts observe ctx — a cancellation
	// landing at the final attempt would otherwise dress itself as clean
	// contention, sending "another holder is busy, retry" guidance to a caller
	// who asked to stop. Cancellation wins.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, false, ctxErr
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

// SleepWithContext waits out delay unless ctx is done first, returning the
// context's error so an interrupted wait surfaces as cancellation rather
// than as a spurious retry outcome. Exported as the one context-aware sleep
// primitive — store's commit-lock retry machinery uses it too, so the
// cancellation semantics of "sleep between attempts" have a single home.
// [LAW:one-source-of-truth]
func SleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
