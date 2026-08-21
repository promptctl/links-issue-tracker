package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The mirror-pending marker is the owned state behind the on-change contract's
// tail guarantee (links-sync-pgct.12): after every mutation, either a
// not-yet-cleared mirror provably still has this commit ahead of its HEAD read,
// or the mutating command spawns that mirror itself. No time constant carries
// the guarantee — the 1s spawn debounce this replaces made the burst's final
// mutation a timing bet on an earlier mirror's HEAD read.
// [LAW:no-ambient-temporal-coupling]
//
// The proof rides on write-engine serialization (links-sync-pgct.11):
// embedded Dolt allows one write-capable engine per path — each holds Dolt's
// own journal lock for its lifetime — so engine sessions are totally
// ordered. The marker is claimed by a mutating command AFTER its own session
// closed (maybeAutoSyncAfterCommand runs post-Close) and cleared by a push
// attempt INSIDE its engine session (performSyncPush entry). A command that
// observes a marker under a live beacon therefore knows the clearing session
// has not run — and since sessions are disjoint, that session's engine open
// (and its HEAD read) lies strictly after this command's committed, closed
// session.
// Covered — conditional on the covering mirror reaching its push attempt.
// The ordering proof is about WHOSE HEAD read covers the commit, never that
// the push lands: a mirror (an observer's borrowed one, or one this command
// spawned itself) can still die before its attempt, and no observable state
// can promise a future push. That arm is the loud-failure contract below —
// every pre-push death clears the marker THROUGH a recorded failed outcome,
// so the stranded tail surfaces on the next mutating command's FAILING
// banner and retries at the next occasion rather than silently waiting.
//
// This marker is deliberately NOT a second representation of push health
// (links-sync-pgct.10's push-outcome.last owns "how did the last attempt
// end"): it answers only "is a mirror still owed", exists transiently between
// a claim and the next push attempt, and failures that break the chain are
// reported through the same pushOutcomeRecord seam as every other attempt.
// [LAW:one-source-of-truth]
//
// Nor does the marker carry liveness. Whether the owing mirror is still
// coming is a separate fact with its own owner — the kernel, through the
// mirror beacon (store.HoldMirrorBeacon / store.ProbeMirrorBeacon): every
// live mirror holds the beacon shared for its whole run, so a claim that
// acquires it exclusively has kernel proof the marker's mirror is gone
// (SIGKILL, machine crash — the endings that run no code; every ending that
// does run code removes or completes the marker itself). No threshold sized
// by summing worst-case healthy delays, no window a loaded machine outgrows:
// death is observed, not estimated. [LAW:no-ambient-temporal-coupling]

// mirrorPendingClaim is the spawn decision derived from the marker.
// [LAW:types-are-the-program] Two legal outcomes, named: the caller never
// re-derives "should I spawn" from raw file state.
type mirrorPendingClaim int

const (
	// pendingCovered: a marker exists and a live mirror holds the beacon, so
	// the marker's not-yet-run clear belongs to a mirror that is still coming
	// — its engine session (and HEAD read) is still ahead of this command's
	// committed, closed session. No spawn needed.
	pendingCovered mirrorPendingClaim = iota
	// pendingClaimed: this command created (or reclaimed from a dead mirror's
	// residue) the marker and now owes the spawn that clears it.
	pendingClaimed
)

// mirrorPendingMarkerPath is the single mirror-pending marker: its existence
// means a mirror is owed, its modification time is when the owing spawn was
// claimed. [LAW:one-source-of-truth]
func mirrorPendingMarkerPath(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "mirror-pending")
}

// claimMirrorPending atomically resolves "is a mirror still owed for commits
// up to now, and if so, who spawns it". The O_EXCL create is the atomicity:
// exactly one of two racing commands creates the marker and owns the spawn;
// the other observes it and is covered by that spawn's mirror — provided the
// mirror still lives, which is the beacon probe's verdict, not a guess from
// the marker's age. A marker whose beacon probe finds no holder is residue:
// either its mirror died running no code (only SIGKILL-class endings leave
// residue — every code-running ending removes or completes the marker), or
// its spawn hasn't reached the beacon yet, in which case the re-claim below
// costs one redundant mirror — the same double-claim the single-flight lock
// already serializes into one push and one cheap no-op. Observers never touch
// the marker's mtime — only a claim does — so the claim stamp keeps meaning
// "when the owed spawn was claimed" for the holder's post-release re-check,
// no matter how busy the workspace is.
func claimMirrorPending(ws workspace.Info, now time.Time) (mirrorPendingClaim, error) {
	if err := os.MkdirAll(ws.StorageDir, 0o755); err != nil {
		return pendingClaimed, fmt.Errorf("ensure storage dir for mirror-pending marker: %w", err)
	}
	path := mirrorPendingMarkerPath(ws)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		if closeErr := file.Close(); closeErr != nil {
			// The create succeeded, so a marker THIS function made now exists —
			// but the error return means the caller never learns it owns one
			// (ensureMirrorCoverage's claimErr branch cannot release it). Remove
			// it here, where the ownership is still known, so a close failure
			// (close-to-open flush on network filesystems) cannot orphan a
			// fresh marker that falsely covers mutations until the next claim's
			// probe. Best-effort: if even the remove fails, the caller's
			// spawn-regardless mirror clears it at its push attempt's entry.
			_ = os.Remove(path)
			return pendingClaimed, fmt.Errorf("close mirror-pending marker: %w", closeErr)
		}
		return pendingClaimed, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return pendingClaimed, fmt.Errorf("claim mirror-pending marker: %w", err)
	}
	alive, probeErr := store.ProbeMirrorBeacon(ws.DatabasePath)
	if probeErr != nil {
		return pendingClaimed, probeErr
	}
	if alive {
		return pendingCovered, nil
	}
	// Kernel-proven residue: no mirror anywhere holds the beacon, so the
	// marker's dedicated mirror is gone (or not yet up — see above; spawning
	// twice is the safe side). Refresh the claim time first so concurrent
	// observers' re-checks bind to THIS re-spawn, then own the spawn. A marker
	// that disappears under the refresh was just cleared by a push attempt
	// inside an engine session — a session that, being disjoint from this
	// command's closed one, opened after this commit: covered.
	if chErr := os.Chtimes(path, now, now); chErr != nil {
		if errors.Is(chErr, os.ErrNotExist) {
			return pendingCovered, nil
		}
		return pendingClaimed, fmt.Errorf("refresh reclaimed mirror-pending marker: %w", chErr)
	}
	return pendingClaimed, nil
}

// clearMirrorPending removes the marker. Two meanings, one operation, both
// truthful removals of "a mirror is owed": a push attempt calls it inside its
// engine session (every commit whose command could have observed this marker
// is inside this session's HEAD read), and a claimant calls it when its claim
// turns out unspawnable (no remote, spawn failure) so later mutations retry
// instead of trusting a mirror that never launched. Already-absent is a
// normal outcome (a racing attempt cleared first); any other failure is loud
// on stderr but never re-colors the caller's own result — a persistently
// unremovable marker degrades to one loudly-recorded push cycle per mutation,
// since each next claim probes the beacon, finds the stopped mirror gone, and
// re-spawns. [LAW:no-silent-failure]
func clearMirrorPending(ws workspace.Info) {
	if err := os.Remove(mirrorPendingMarkerPath(ws)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "lit: mirror-pending marker not cleared: %v\n", err)
	}
}

// mirrorPendingSet reports whether any marker exists, live holder or not. Test
// observability for the claim lifecycle; the mirror loop's own re-check is
// recheckMirrorPending, which additionally proves the marker is a NEW claim.
func mirrorPendingSet(ws workspace.Info) bool {
	_, err := os.Stat(mirrorPendingMarkerPath(ws))
	return err == nil
}

// recheckMirrorPending is the single-flight holder's post-release verdict:
// again=true means a claim landed after cycleStart (stamped after the cycle's
// entry-clear ran, so its claimant may sit behind the cycle's HEAD read) and
// the holder owes another cycle. Liveness is irrelevant here — any new claim
// means commits may be uncovered, whoever claimed it, and cycling on a dead
// claimant's residue recovers it for free.
//
// A non-nil error is a STOP, not a retry: a marker older than cycleStart
// survived the cycle's own entry-clear, meaning the clear is failing (a
// read-only storage dir whose engine and push still work), and a loop keyed on
// bare existence would run full push cycles forever against a marker it can
// never remove; an unreadable marker gives the loop no truthful basis either
// way. Both endings leave the claim to the next mutation's own claim (or its
// beacon-probe residue recovery), with the error surfaced by the caller
// rather than a silent exit dropping custody. [LAW:no-silent-failure]
func recheckMirrorPending(ws workspace.Info, cycleStart time.Time) (again bool, err error) {
	info, statErr := os.Stat(mirrorPendingMarkerPath(ws))
	if errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	if statErr != nil {
		return false, fmt.Errorf("re-check mirror-pending marker: %w", statErr)
	}
	if info.ModTime().Before(cycleStart) {
		return false, fmt.Errorf(
			"mirror-pending marker from %s survived this cycle's clear (started %s); stopping rather than cycling against a marker that cannot be removed",
			info.ModTime().Format(time.RFC3339), cycleStart.Format(time.RFC3339))
	}
	return true, nil
}
