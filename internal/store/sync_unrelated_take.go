package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// takeLocalCommitMessage labels the marker commit that settles a take-local
// resolution's forward replay onto the remote head, so the linear history names
// what produced it — distinct from the field-aware merge's
// reconcileCommitMessage. Its diff is the take's own act: the discard of the
// remote-only issues the owner approved dropping.
const takeLocalCommitMessage = "reconcile: take local backlog over unrelated remote history"

// takeApprovalToken derives the owner-approval token that authorizes destroying
// one side of THIS exact unrelated-history fork: a digest of both heads and the
// chosen side, the prose-resolve Fingerprint device lifted from field scope to
// whole-backlog scope. The owner approves a specific destruction — these ids, at
// these heads, this side — so any commit on either side (or approving one side
// and running the other) changes the token and voids the approval.
// [LAW:types-are-the-program] "the owner saw THIS fork and chose" becomes a
// checkable value rather than an assumption. Deliberately unexported: the only
// way to obtain a token is to run the take and read its refusal, so the
// destruction inventory has been surfaced before any approval can exist.
func takeApprovalToken(localHead, remoteHead string, choice UnrelatedResolution) string {
	sum := sha256.Sum256([]byte("take:" + string(choice) + "\x00" + localHead + "\x00" + remoteHead))
	return hex.EncodeToString(sum[:6])
}

// OwnerApprovalRequiredError is the take gate's refusal: the divergence is real
// and takeable, but the supplied approval does not match this exact fork+side,
// so nothing was mutated. It carries everything the surface needs to put the
// decision in front of the OWNER — the current token, both heads, the
// both-sides inventory of what the take would keep and destroy — because the
// party who can lose work is the party who must authorize the loss.
// [LAW:parse-dont-validate] the error is the checkpoint's output: downstream
// rendering needs no second store read to describe the refusal.
type OwnerApprovalRequiredError struct {
	Choice UnrelatedResolution
	// ApprovalToken is the token that WOULD authorize this take right now.
	ApprovalToken string
	// Stale is true when a token was supplied but no longer matches — the
	// backlog moved since it was issued, or it was issued for the other side.
	Stale                 bool
	LocalHead, RemoteHead string
	Ahead, Behind         int64
	Inventory             *UnrelatedInventory
}

func (e OwnerApprovalRequiredError) Error() string {
	if e.Stale {
		return fmt.Sprintf("sync reconcile take %s: the supplied owner approval no longer matches this divergence (the backlog moved, or it was issued for the other side)", e.Choice)
	}
	return fmt.Sprintf("sync reconcile take %s is destructive and requires explicit owner approval", e.Choice)
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
// It is snapshot-guarded and GC-retry-wrapped like the three-way reconcile — the take
// is destructive, so a pre-mutation recovery point matters even more here. take-local,
// which authors replay commits on the remote head, also shares the three-way path's
// schema-ahead refusal (guardCommitSchemaAhead); take-remote, which adopts the remote
// head wholesale, authors no replay commit and is exempt. CONSTRAINT (embedded Dolt
// one-RW-engine-per-path): runs INLINE/foreground on the caller's own engine, never a
// background worker.
//
// ownerApproval is the owner-confirmation step (links-sync-pgct.4): the take runs
// only when it matches this exact fork+side's takeApprovalToken. Anything else —
// absent, stale, or issued for the other side — returns OwnerApprovalRequiredError
// carrying the current token and inventory, with nothing mutated. The check runs
// under the same commit lock as the mutation, so there is no gap between the state
// the owner approved and the state destroyed. [LAW:no-ambient-temporal-coupling]
// [LAW:single-enforcer] the gate lives on the destructive operation itself, not on
// any of its surfaces.
func (s *Store) SyncResolveUnrelated(ctx context.Context, remote string, branch string, choice UnrelatedResolution, ownerApproval string) (SyncReconcileResult, error) {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return SyncReconcileResult{}, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return SyncReconcileResult{}, err
	}
	if !choice.Valid() {
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

		// The owner-confirmation gate, after the inventory read so the refusal can
		// name exactly what the take would destroy, and before any mutation.
		expected := takeApprovalToken(plan.localHead, plan.remoteHead, choice)
		if ownerApproval != expected {
			return OwnerApprovalRequiredError{
				Choice:        choice,
				ApprovalToken: expected,
				Stale:         ownerApproval != "",
				LocalHead:     plan.localHead,
				RemoteHead:    plan.remoteHead,
				Ahead:         plan.ahead,
				Behind:        plan.behind,
				Inventory:     inventory,
			}
		}

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
	switch choice {
	case TakeRemote:
		// take-remote adopts the remote head wholesale (resetHardToRef) — no scratch, no
		// replay commit — so it takes only its own snapshot guard, NOT the full scratch
		// envelope, and is exempt from the schema-ahead refusal: adopting an ahead head is a
		// safe recovery (the stale binary gets the newer data), not a regression.
		guard := newSnapshotGuard(s.doltRootDir, migrationSnapshotsDir(s.doltRootDir), formatReconcileSnapshotLabel(time.Now()))
		trackingRef := fmt.Sprintf("remotes/%s/%s", remote, branch)
		return retryTransientGCContention(ctx, func(ctx context.Context) error {
			return s.takeRemoteHead(ctx, result, guard, trackingRef)
		}, s.reconnect, transientRetryDelay, waitWithContext)
	case TakeLocal:
		// take-local authors replay commits ON the remote head (commitReplayAndAdvance),
		// so it shares the three-way path's full safe-replay envelope — schema-ahead refusal
		// included: a replay below an ahead remote's schema would drop the newer fields and
		// regress the shared remote on push (the 2026-07-08 incident shape).
		// [LAW:single-enforcer] the same replayUnderGuard that wraps the three-way reconcile.
		return s.replayUnderGuard(ctx, remote, branch, plan.remoteHead, func(ctx context.Context, guard *snapshotGuard, scratch reconcileScratch) error {
			return s.takeLocalOntoRemoteHead(ctx, result, guard, plan.dataBranch, scratch, plan.localHead, plan.remoteHead)
		})
	default:
		// SyncResolveUnrelated already rejected an invalid value at its boundary, so this
		// is unreachable. The union (combine) resolution is NOT a wholesale take — it merges
		// rather than picks a side — so it lives on the reconcile boundary (SyncReconcileCombine)
		// and never reaches this dispatch; any unhandled value here still fails loudly rather
		// than silently no-op. [LAW:no-silent-failure]
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
	if _, err := guard.ensure(ctx); err != nil {
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

// takeLocalOntoRemoteHead replays the local backlog forward onto the remote head, so
// local becomes a fast-forwardable descendant carrying its own content — the mirror
// of take-remote, built to converge the remote onto local on the next push (the local
// branch pointer is all this store can move directly; it reaches the remote only
// through a fast-forward). The local chain's commits land individually under their
// own provenance, projected as unions with the remote content exactly as a combine
// projects them — so mid-chain history stays a whole backlog — and the take's marker
// commit settles on local's own content: ITS diff is the discard of the remote-only
// issues, attributing the owner-approved destruction to the take itself rather than
// smearing it into local's first commit. It reuses the exact safe-replay the
// three-way reconcile uses: scratch branch, snapshot-first, one atomic data-branch
// advance. [LAW:single-enforcer] one safe-replay, one step builder, one scratch
// lifecycle, shared with the field-aware path.
func (s *Store) takeLocalOntoRemoteHead(ctx context.Context, result *SyncReconcileResult, guard *snapshotGuard, dataBranch string, scratch reconcileScratch, localHead, remoteHead string) error {
	return s.runOnReconcileScratch(ctx, dataBranch, scratch, localHead, func() error {
		local, err := s.exportAtCommit(ctx, scratch.read, localHead)
		if err != nil {
			return err
		}
		theirs, err := s.exportAtCommit(ctx, scratch.read, remoteHead)
		if err != nil {
			return err
		}
		chain, err := readFoldedChain(ctx, s.db, remoteHead, localHead)
		if err != nil {
			return err
		}
		// The no-base union projection combine uses; only the terminal export
		// differs — the take settles on local's content, not the union.
		// [LAW:dataflow-not-control-flow]
		stepper := foldStepper{store: s, readBranch: scratch.read, chain: chain, base: model.Export{}, theirs: theirs}
		replayed, err := s.commitReplayAndAdvance(ctx, guard, dataBranch, scratch, remoteHead, takeLocalCommitMessage, local, stepper)
		if err != nil {
			return err
		}
		result.State = SyncReconcileTookLocal
		result.Replayed = replayed
		return nil
	})
}
