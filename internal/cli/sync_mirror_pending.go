package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
// own journal lock for its lifetime — so engine sessions are totally ordered. The marker is claimed by a mutating command AFTER its own session
// closed (maybeAutoSyncAfterCommand runs post-Close) and cleared by a push
// attempt INSIDE its engine session (performSyncPush entry). A command that
// observes a fresh marker therefore knows the clearing session has not run —
// and since sessions are disjoint, that session's engine open (and its HEAD
// read) lies strictly after this command's committed, closed session. Covered,
// by construction.
//
// This marker is deliberately NOT a second representation of push health
// (links-sync-pgct.10's push-outcome.last owns "how did the last attempt
// end"): it answers only "is a mirror still owed", exists transiently between
// a claim and the next push attempt, and failures that break the chain are
// reported through the same pushOutcomeRecord seam as every other attempt.
// [LAW:one-source-of-truth]
const (
	// mirrorPendingStaleAfter is crash recovery, not the coverage carrier: a
	// marker older than this had its dedicated mirror die before reaching a
	// push attempt (SIGKILL, machine crash — the loud failure paths remove or
	// complete the marker themselves), so a claim treats it as unowned and
	// re-spawns. Sized above the worst healthy claim-to-clear chain: the
	// mirror's 30s parent-exit wait, a 30s engine-open budget behind a slow
	// foreground command, and a full predecessor push cycle before the
	// single-flight holder's post-release re-check picks the marker up. A
	// too-early fire costs one redundant spawn (the single-flight lock and an
	// up-to-date push make it a no-op); a crashed mirror's residue costs at
	// most this window before the next mutation re-claims.
	mirrorPendingStaleAfter = 5 * time.Minute
)

// mirrorPendingClaim is the spawn decision derived from the marker.
// [LAW:types-are-the-program] Two legal outcomes, named: the caller never
// re-derives "should I spawn" from raw file state.
type mirrorPendingClaim int

const (
	// pendingCovered: a fresh marker exists, so its dedicated mirror has not
	// cleared it — that mirror's engine session (and HEAD read) is still ahead
	// of this command's committed, closed session. No spawn needed.
	pendingCovered mirrorPendingClaim = iota
	// pendingClaimed: this command created (or stale-refreshed) the marker and
	// now owes the spawn that clears it.
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
// the other observes it fresh and is covered by that spawn's mirror. A marker
// that disappears between the create attempt and the stat was just cleared by
// a push attempt inside an engine session — a session that, being disjoint
// from this command's closed one, opened after this commit: covered.
// Observers never touch the marker's mtime — only a claim does — so a dead
// mirror's residue ages into mirrorPendingStaleAfter no matter how busy the
// workspace is, instead of being kept forever fresh by the very mutations it
// is stranding.
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
			// fresh marker that falsely covers mutations for the staleness
			// window. Best-effort: if even the remove fails, the caller's
			// spawn-regardless mirror clears it at its push attempt's entry.
			_ = os.Remove(path)
			return pendingClaimed, fmt.Errorf("close mirror-pending marker: %w", closeErr)
		}
		return pendingClaimed, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return pendingClaimed, fmt.Errorf("claim mirror-pending marker: %w", err)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return pendingCovered, nil
		}
		return pendingClaimed, fmt.Errorf("stat mirror-pending marker: %w", statErr)
	}
	// Negative age — a marker stamped in the future — can only be a crash
	// orphan seen across a backward clock step (an SIGKILLed claimant, then an
	// RTC/NTP correction): claims are stamped from this machine's one clock,
	// so no live claimant writes ahead of now. Reading it as covered would
	// suppress every spawn until wall clock caught up; reading it as stale
	// costs at most one redundant spawn. [LAW:no-silent-failure]
	if age := now.Sub(info.ModTime()); age >= 0 && age < mirrorPendingStaleAfter {
		return pendingCovered, nil
	}
	// Crash recovery: the marker's dedicated mirror died without clearing or
	// completing. Refresh the claim time first so concurrent observers bind to
	// THIS re-spawn, then own the spawn. Two commands racing the same stale
	// marker may both claim — two mirrors, serialized by the single-flight
	// lock into one push and one cheap no-op. Harmless, and rarer than rare.
	if chErr := os.Chtimes(path, now, now); chErr != nil {
		if errors.Is(chErr, os.ErrNotExist) {
			return pendingCovered, nil
		}
		return pendingClaimed, fmt.Errorf("refresh stale mirror-pending marker: %w", chErr)
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
// on stderr but never re-colors the caller's own result — a persistent
// failure here degrades to one push attempt per staleness window, each one
// still loudly recorded. [LAW:no-silent-failure]
func clearMirrorPending(ws workspace.Info) {
	if err := os.Remove(mirrorPendingMarkerPath(ws)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "lit: mirror-pending marker not cleared: %v\n", err)
	}
}

// mirrorPendingSet reports whether any marker exists, fresh or stale. Test
// observability for the claim lifecycle; the mirror loop's own re-check is
// recheckMirrorPending, which additionally proves the marker is a NEW claim.
func mirrorPendingSet(ws workspace.Info) bool {
	_, err := os.Stat(mirrorPendingMarkerPath(ws))
	return err == nil
}

// recheckMirrorPending is the single-flight holder's post-release verdict:
// again=true means a claim landed after cycleStart (stamped after the cycle's
// entry-clear ran, so its claimant may sit behind the cycle's HEAD read) and
// the holder owes another cycle. Staleness is irrelevant here — any new claim
// means commits may be uncovered, and cycling on a stale one recovers a
// crashed claimant's residue for free.
//
// A non-nil error is a STOP, not a retry: a marker older than cycleStart
// survived the cycle's own entry-clear, meaning the clear is failing (a
// read-only storage dir whose engine and push still work), and a loop keyed on
// bare existence would run full push cycles forever against a marker it can
// never remove; an unreadable marker gives the loop no truthful basis either
// way. Both endings leave the claim to the next mutation's own claim (or
// staleness recovery), with the error surfaced by the caller rather than a
// silent exit dropping custody. [LAW:no-silent-failure]
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
