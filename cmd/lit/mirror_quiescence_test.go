//go:build !windows

package main

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

const (
	// mirrorQuiescencePatience bounds the join. A mirror still owed at cleanup
	// has a real cycle ahead of it — engine open, push attempt, engine close —
	// and the holder's post-release re-check can chain a second one, each of
	// which is slow on a loaded CI machine under -race. The same budget the
	// burst test's delivery poll uses, and for the same reason: generous
	// headroom costs nothing when healthy, because the wait ends the instant
	// the workspace reads quiescent.
	mirrorQuiescencePatience     = 60 * time.Second
	mirrorQuiescencePollInterval = 20 * time.Millisecond
)

// awaitMirrorQuiescence makes a test's TempDir sweep safe against the detached
// on-change mirror by joining it — waiting on the mirror's own kernel-visible
// state, never on a clock. [LAW:no-ambient-temporal-coupling]
//
// A test that enables auto-sync leaves its LAST spawned mirror running when the
// test body returns. Detached by design, that process writes under
// <root>/.git/links exactly while t.TempDir's cleanup sweeps the directory, and
// an entry created between RemoveAll's readdir and its rmdir fails the sweep
// with "directory not empty". Every write it can make counts, down to the
// smallest: filelock.Acquire opens its lock path O_CREATE, so even a mirror
// that only loses the single-flight race and exits without opening a store
// still creates two files here — the beacon and the sync-push lock. There is
// no branch of the mirror that is safe to race, so the cleanup waits for the
// process to be GONE rather than for it to be harmless.
//
// Two observations answer that, and the ORDER of the two is the correctness:
//
//   - the mirror-pending marker, read FIRST: it is stamped by the claimant
//     BEFORE the spawn and cleared only by a mirror that has already entered
//     its cycle, so it is the only evidence covering a mirror between
//     cmd.Start and its own first instruction — a live process the kernel
//     cannot yet show anyone;
//   - the mirror beacon, probed LAST: every mirror holds it shared from process
//     entry until it dies, by any death mode, so BeaconUnheld is kernel proof
//     that no mirror (and no claimant) was running at that instant.
//
// Deciding on the beacon last is what makes the pair sound, the same discipline
// ProbeMirrorBeacon applies internally. Reversed, a mirror could enter and clear
// the marker in between the two reads, and the pair would report quiescence
// over a process that is running. Read in this order, a mirror still in its
// spawn window is caught by the marker it has not yet cleared, and one that has
// entered is caught by the beacon it is holding.
//
// The single-flight sync-push lock is deliberately NOT part of the join. Taking
// it would not silence a late mirror — it would elect the one branch that still
// creates lock files — and holding it through the sweep starves the marker's
// only clearer, so the join could never complete.
//
// One residual, stated rather than implied, because the pair covers a single
// spawn exactly and two overlapping ones only nearly: racing claims can put a
// SECOND mirror in its spawn window while a FIRST one is running, and the
// first's cycle-entry clear removes the one marker both are behind. A probe
// landing after the first has exited and before the second has run an
// instruction would read quiescent over a live process. It needs the second
// mirror to stay unscheduled across the first's entire engine cycle, and it is
// the same shape as the beacon's own documented residual — no observable state
// can prove a process that has not run yet. Sequential mutations, which is what
// these tests drive, produce one mirror at a time and never reach it.
func awaitMirrorQuiescence(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		ws, err := workspace.Resolve(root)
		if err != nil {
			t.Errorf("mirror-quiescence cleanup: resolve workspace: %v", err)
			return
		}
		deadline := time.Now().Add(mirrorQuiescencePatience)
		for {
			owed, err := cli.MirrorOwed(ws)
			if err != nil {
				t.Errorf("mirror-quiescence cleanup: %v", err)
				return
			}
			verdict, err := store.ProbeMirrorBeacon(ws.DatabasePath)
			if err != nil {
				t.Errorf("mirror-quiescence cleanup: %v", err)
				return
			}
			if !owed && verdict == store.BeaconUnheld {
				return
			}
			if time.Now().After(deadline) {
				// Loud, because the sweep that runs next is now known to be
				// unguarded: reporting the RemoveAll error it produces would
				// name the symptom, and this names the cause.
				// [LAW:no-silent-failure]
				t.Errorf("workspace never reached mirror quiescence within %s (mirror owed: %t, beacon verdict: %s); the TempDir sweep will race a live mirror\n%s",
					mirrorQuiescencePatience, owed, verdict, dumpMirrorLog(root))
				return
			}
			time.Sleep(mirrorQuiescencePollInterval)
		}
	})
}
