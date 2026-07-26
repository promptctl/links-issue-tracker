package store

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
)

// takeLocalCommitMessage labels the single forward commit a take-local resolution
// replays onto the remote head, so the linear history names what produced it —
// distinct from the field-aware merge's reconcileCommitMessage.
const takeLocalCommitMessage = "reconcile: take local backlog over unrelated remote history"

// UnrelatedResolution is the whole-store choice that settles an unrelated-history
// divergence: take the local backlog wholesale, or take the remote backlog
// wholesale. It is the field-level resolve concept (internal/merge, twoTier's Tier-2
// pick between two sides that both moved) lifted from field scope to the whole store:
// with no merge-base every issue is "changed on both sides from empty", so the only
// resolution is which side to take entire. The epic's later `combine` is a third
// value of this same type, flowing through the same reconcile boundary, never a
// parallel mode. [LAW:types-are-the-program] the value carries the whole decision;
// there is no legal take with no side.
type UnrelatedResolution string

const (
	// TakeLocal keeps the local backlog and discards the remote-only issues; the
	// remote-tracking side converges to local on the next push.
	TakeLocal UnrelatedResolution = "local"
	// TakeRemote keeps the remote backlog and discards the local-only issues; local
	// content becomes equal to the remote head.
	TakeRemote UnrelatedResolution = "remote"
)

// valid reports whether r is a resolution this binary can apply. It is the boundary
// guard SyncResolveUnrelated runs before touching the store, so an unknown side is
// rejected loudly at the door rather than silently no-op'd at the dispatch.
// [LAW:no-silent-failure]
func (r UnrelatedResolution) valid() bool {
	return r == TakeLocal || r == TakeRemote
}

// SyncResolveUnrelated settles an unrelated-history divergence by taking one side
// wholesale. It reads the SAME decision state the field-aware reconcile reads —
// captureReconcilePlan under the commit lock — and requires an unrelated divergence:
// a divergence WITH a common base is mergeable and must go through SyncReconcile,
// which preserves both sides' non-conflicting work rather than discarding a side, so
// this refuses it loudly rather than reset a mergeable backlog. [LAW:no-silent-failure]
//
// The chosen side's unique issues survive and the OTHER side's unique issues are
// discarded BY DESIGN; the both-sides inventory is read off the two anchors and
// carried on the result so the surface can report exactly what was dropped, never
// silently. [FRAMING:representation] the result names the discard.
//
// It is snapshot-guarded and GC-retry-wrapped exactly like the three-way reconcile —
// the take is destructive, so a pre-mutation recovery point matters even more here.
// CONSTRAINT (embedded Dolt one-RW-engine-per-path): runs INLINE/foreground on the
// caller's own engine, never a background worker.
func (s *Store) SyncResolveUnrelated(ctx context.Context, remote string, branch string, choice UnrelatedResolution) (SyncReconcileResult, error) {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return SyncReconcileResult{}, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return SyncReconcileResult{}, err
	}
	if !choice.valid() {
		// [LAW:no-silent-failure] an unknown side must never reach the dispatch and
		// silently do nothing; reject it at the boundary with the legal values.
		return SyncReconcileResult{}, fmt.Errorf("resolve unrelated histories: unknown side %q (want %q or %q)", choice, TakeLocal, TakeRemote)
	}

	var result SyncReconcileResult
	err = s.withCommitLock(ctx, func(ctx context.Context) error {
		plan, err := s.captureReconcilePlan(ctx, trimmedRemote, trimmedBranch)
		if err != nil {
			return err
		}
		result.Ahead, result.Behind = plan.ahead, plan.behind
		if !plan.diverged {
			// Nothing to resolve — a re-run after the divergence already cleared lands
			// here. [LAW:dataflow-not-control-flow] the freshness value selects the outcome.
			result.State = SyncReconcileNotDiverged
			return nil
		}
		_, shared := plan.base.shared()
		result.LocalHead, result.RemoteHead = plan.localHead, plan.remoteHead
		if shared {
			// A divergence WITH a base is mergeable; taking one side wholesale here would
			// silently drop the other side's non-conflicting work that the field-aware
			// reconcile would have kept. Refuse and route to the merge. [LAW:no-silent-failure]
			return fmt.Errorf("sync reconcile take applies only to unrelated histories, but %s/%s shares history with the local backlog; run `lit sync reconcile` to field-merge it", trimmedRemote, trimmedBranch)
		}

		// Read the both-sides inventory off the two anchors while under the lock — pure
		// AS OF reads that move no branch — so the result can report exactly which
		// side's unique issues are discarded. [LAW:no-ambient-temporal-coupling]
		inventory, err := s.unrelatedInventory(ctx, plan.localHead, plan.remoteHead)
		if err != nil {
			return err
		}
		result.Unrelated = inventory

		return s.applyUnrelatedTake(ctx, &result, plan, trimmedRemote, trimmedBranch, choice)
	})
	if err != nil {
		return SyncReconcileResult{}, err
	}
	return result, nil
}

// applyUnrelatedTake dispatches the wholesale take on the resolution value, sharing
// one snapshot guard across the GC-contention retry so exactly one recovery point is
// taken however many attempts run. Each side owns its own mutation: take-remote resets
// the data branch to the remote head; take-local replays the local export as a forward
// commit on the remote head. [LAW:dataflow-not-control-flow] the choice is a value
// selecting the domain operation, not a mode threaded through duplicated plumbing.
func (s *Store) applyUnrelatedTake(ctx context.Context, result *SyncReconcileResult, plan reconcilePlan, remote, branch string, choice UnrelatedResolution) error {
	guard := newSnapshotGuard(s.doltRootDir, migrationSnapshotsDir(s.doltRootDir), formatReconcileSnapshotLabel(time.Now()))
	switch choice {
	case TakeRemote:
		trackingRef := fmt.Sprintf("remotes/%s/%s", remote, branch)
		return retryTransientGCContention(ctx, func(ctx context.Context) error {
			return s.takeRemoteHead(ctx, result, guard, trackingRef)
		}, s.reconnect, transientRetryDelay, waitWithContext)
	case TakeLocal:
		// Sweep any scratch branch a previously-killed run abandoned, then derive this
		// run's unique name; the commit lock guarantees any existing scratch is an orphan.
		s.sweepStaleReconcileScratch(ctx)
		scratchBranch := reconcileScratchName()
		return retryTransientGCContention(ctx, func(ctx context.Context) error {
			return s.takeLocalOntoRemoteHead(ctx, result, guard, plan.dataBranch, scratchBranch, plan.localHead, plan.remoteHead)
		}, s.reconnect, transientRetryDelay, waitWithContext)
	default:
		// SyncResolveUnrelated already rejected an invalid value at its boundary, so this
		// is unreachable — but a future third resolution (combine) reaching here unhandled
		// must fail loudly, never silently no-op. [LAW:no-silent-failure]
		return fmt.Errorf("resolve unrelated histories: unhandled side %q", choice)
	}
}

// takeRemoteHead adopts the remote head wholesale: it snapshots the pre-reset local
// truth (so this destructive adopt is reversible via `lit snapshots restore`, exactly
// as the three-way reconcile snapshots before it advances the data branch), resets the
// data branch to the remote head, then rolls the recovery snapshots off to budget.
// Local content then equals the remote and sync is clean; the local-only issues are
// discarded by design. It is idempotent under retry: the snapshot is cached and the
// reset targets a fixed ref. [LAW:no-silent-failure]
func (s *Store) takeRemoteHead(ctx context.Context, result *SyncReconcileResult, guard *snapshotGuard, trackingRef string) error {
	if _, err := guard.ensure(); err != nil {
		return fmt.Errorf("snapshot before take-remote: %w", err)
	}
	if err := resetHardToRef(ctx, s.db, trackingRef); err != nil {
		return err
	}
	// Prune runs AFTER the durable reset, so a prune failure must not fail a completed
	// take (a retry would find nothing to do). A leftover snapshot is inert.
	// [LAW:no-silent-failure] surfaced, not promoted.
	if err := dbsnapshot.PruneMatching(migrationSnapshotsDir(s.doltRootDir), reconcileSnapshotRetention, IsReconcileSnapshotName); err != nil {
		fmt.Fprintf(os.Stderr, "lit: take-remote could not prune old recovery snapshots (reset already done): %v\n", err)
	}
	result.State = SyncReconcileTookRemote
	return nil
}

// takeLocalOntoRemoteHead replays the local backlog as one forward commit on the
// remote head, so local becomes a fast-forwardable descendant carrying its own content
// — the mirror of take-remote, built to converge the remote onto local on the next
// push (the local branch pointer is all this store can move directly; it reaches the
// remote only through a fast-forward). It reuses the exact safe-replay the three-way
// reconcile uses: scratch branch, snapshot-first, one atomic data-branch advance. The
// export is local's own content read at localHead, so the remote-only issues are
// absent from it — discarded by design. [LAW:single-enforcer] one safe-replay, one
// scratch lifecycle, shared with the field-aware path.
func (s *Store) takeLocalOntoRemoteHead(ctx context.Context, result *SyncReconcileResult, guard *snapshotGuard, dataBranch, scratchBranch, localHead, remoteHead string) error {
	return s.runOnReconcileScratch(ctx, dataBranch, scratchBranch, localHead, func() error {
		local, err := s.exportAtCommit(ctx, localHead)
		if err != nil {
			return err
		}
		if err := s.commitReplayAndAdvance(ctx, guard, dataBranch, remoteHead, takeLocalCommitMessage, local); err != nil {
			return err
		}
		result.State = SyncReconcileTookLocal
		return nil
	})
}
