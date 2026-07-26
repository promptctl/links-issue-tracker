package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// reconcileCommitMessage labels the single forward commit a settled reconcile
// replays onto the remote head, so the linear history names what produced it.
const reconcileCommitMessage = "reconcile: field-aware merge of remote divergence"

// reconcileScratchPrefix names the throwaway branches the reconcile builds its
// merged commit on. Each reconcile derives a UNIQUE branch under this prefix, so
// cleanup only ever touches a branch this run created — never an unrelated branch
// that happened to share a fixed name. [LAW:locality-or-seam] the scratch ref is
// this run's private seam. A startup sweep drops any leftovers a killed run
// abandoned, so unique names cannot accumulate.
const reconcileScratchPrefix = "links-reconcile-scratch"

// reconcileScratchName derives this reconcile's unique scratch branch from the
// process id and a high-resolution timestamp. The commit lock already serializes
// reconciles within and across processes, so this is belt-and-suspenders against
// ever touching a branch another context owns. [LAW:effects-at-boundaries] the one
// nondeterministic input (the clock) is read here, at the boundary, not threaded
// through the pure merge.
func reconcileScratchName() string {
	return fmt.Sprintf("%s-%d-%d", reconcileScratchPrefix, os.Getpid(), time.Now().UnixNano())
}

// reconcileSnapshotLabel is the label prefix every reconcile-recovery snapshot
// carries, disjoint from the migration and downgrade labels so the three
// producers' snapshots roll off under independent retention budgets and never
// collect each other. [LAW:one-source-of-truth]
const reconcileSnapshotLabel = "pre-reconcile"

// reconcileSnapshotRetention bounds how many reconcile-recovery snapshots are
// kept on disk; older ones roll off via PruneMatching after a successful
// reconcile. It matches the migration/downgrade budgets — a reconcile is a rare,
// divergence-only event, so a small history of recovery points is ample.
const reconcileSnapshotRetention = 10

// IsReconcileSnapshotName reports whether name was stamped by a reconcile (vs.
// migrate(), Downgrade, `lit snapshots new`, or any other producer). It shares
// the exact stamped-shape rule with the migration and downgrade classifiers.
// [LAW:types-are-the-program] the predicate is the exact shape reconcile stamps;
// a match by accident is impossible without a user mimicking the format.
func IsReconcileSnapshotName(name string) bool {
	return isStampedSnapshotName(name, reconcileSnapshotLabel)
}

// formatReconcileSnapshotLabel returns the label a reconcile-recovery snapshot
// carries: the trailing timestamp is cosmetic (dbsnapshot.Take already encodes
// the take-time) but makes the snapshot readable in operator listings.
func formatReconcileSnapshotLabel(t time.Time) string {
	return fmt.Sprintf("%s-%d", reconcileSnapshotLabel, t.UTC().UnixNano())
}

// SyncReconcileState classifies what a single foreground reconcile did with a
// diverged local branch. [LAW:one-source-of-truth] One mapping from the engine's
// outcome to a label; the CLI renders this, it never re-derives it.
type SyncReconcileState string

const (
	// SyncReconcileNotDiverged: the branch is not diverged (resolved by a push
	// race, or it never diverged). Nothing to reconcile; the caller's other
	// freshness states own those paths.
	SyncReconcileNotDiverged SyncReconcileState = "not_diverged"
	// SyncReconcileLinearized: the field-aware engine resolved every field; the
	// merged result was replayed as one forward commit on the remote head, leaving
	// linear history with no merge commit, so the next push fast-forwards.
	SyncReconcileLinearized SyncReconcileState = "linearized"
	// SyncReconcileProsePending: the engine settled every code-owned field, but at
	// least one free-text field diverged on both sides. Nothing is committed and
	// the local branch is left untouched (still diverged); the prose conflicts are
	// returned for the agent surface to merge. [LAW:no-silent-failure] a divergence
	// the engine cannot resolve alone is surfaced, never auto-committed by picking
	// a side.
	SyncReconcileProsePending SyncReconcileState = "prose_pending"
	// SyncReconcileUnrelated: the local branch and the remote-tracking ref share no
	// common ancestor — independently-created stores, or one that was re-inited — so
	// there is no base for a three-way merge. The reconcile DETECTS this before any
	// write and commits nothing: the three-way path assumes a base, and driving an
	// absent one into it fails obscurely (an empty/no-row merge-base, not a clear
	// diagnosis). The divergence is real but unmergeable by the base-assuming engine;
	// it is surfaced for the wholesale/union resolution the rest of this epic builds,
	// never crashed through an empty merge-base. [LAW:no-silent-failure]
	SyncReconcileUnrelated SyncReconcileState = "unrelated_histories"
	// SyncReconcileTookLocal: the operator resolved an unrelated-history divergence by
	// taking the LOCAL side wholesale. The local backlog was replayed as one forward
	// commit on the remote head, so it is now a fast-forwardable descendant the next
	// push converges the remote onto; the remote-only issues were discarded by design.
	// Only SyncResolveUnrelated produces this — the autonomous reconcile never picks a
	// side. [LAW:no-silent-failure] a data-dropping resolution is only ever a deliberate
	// choice, never the automatic path.
	SyncReconcileTookLocal SyncReconcileState = "took_local"
	// SyncReconcileTookRemote: the operator resolved an unrelated-history divergence by
	// taking the REMOTE side wholesale. The local branch was reset to the remote head,
	// so local content now equals the remote and sync is clean; the local-only issues
	// were discarded by design. Only SyncResolveUnrelated produces this.
	SyncReconcileTookRemote SyncReconcileState = "took_remote"
)

// SyncReconcileResult reports the reconcile outcome, the ahead/behind counts it
// was decided from, and the three commit anchors it merged. Pending is non-empty
// only for SyncReconcileProsePending.
type SyncReconcileResult struct {
	State      SyncReconcileState
	Ahead      int64
	Behind     int64
	LocalHead  string
	RemoteHead string
	BaseCommit string
	// Pending carries the free-text fields that diverged on both sides, with
	// base/ours/theirs, so the agent surface can merge intent instead of picking a
	// side. Empty unless State is SyncReconcileProsePending.
	Pending []merge.ProsePending
	// Unrelated carries the both-sides issue-id partition (only-local, only-remote,
	// on-both) so the operator can see what each side holds before choosing a
	// wholesale/union resolution. Non-nil only for SyncReconcileUnrelated; there is
	// no base to diff against, so it is read directly off the LocalHead/RemoteHead
	// anchors. [LAW:types-are-the-program] the field present names the state that
	// produced it.
	Unrelated *UnrelatedInventory
}

// settleFn turns the three-way merge of a divergence into the export to replay as
// the single forward commit, OR a non-empty pending set that holds the reconcile
// for the agent surface. It is the ONLY thing that differs between the autonomous
// reconcile (which can never resolve prose itself) and the agent-resolved finalize
// (which splices the agent's merged text in): the lock, freshness gate, anchor
// capture, scratch branch, and atomic reparent are identical for both, so they
// live in one shared core and the policy crosses the seam as a value.
// [LAW:dataflow-not-control-flow] the per-path variability is this value, not a
// branch duplicated through the safety-critical replay. [LAW:single-enforcer] the
// scratch-branch safe-replay is written once.
type settleFn func(merge.MergeResult) (model.Export, []merge.ProsePending)

// autonomousSettle is the reconcile's own policy: commit the merge only when every
// field — prose included — converged without the agent; otherwise hold the live
// prose conflicts. [LAW:no-silent-failure] prose is never auto-committed by picking
// a side.
func autonomousSettle(merged merge.MergeResult) (model.Export, []merge.ProsePending) {
	if export, ok := merged.Settled(); ok {
		return export, nil
	}
	return model.Export{}, merged.Pending
}

// resolvedSettle is the finalize policy: splice the agent's merged prose into the
// provisional export and commit it. It first honors a divergence that converged on
// its own between the agent reading and finalizing (Settled), then applies the
// agent's resolutions — but only when they are an exact bijection with the LIVE
// pending set. A stale or partial set falls back to holding the CURRENT pending so
// the caller re-surfaces it; the agent never commits against a divergence that
// changed underneath it. [LAW:no-silent-failure]
func resolvedSettle(resolutions []merge.ProseResolution) settleFn {
	return func(merged merge.MergeResult) (model.Export, []merge.ProsePending) {
		if export, ok := merged.Settled(); ok {
			return export, nil
		}
		if export, ok := merge.ApplyProseResolutions(merged, resolutions); ok {
			return export, nil
		}
		return model.Export{}, merged.Pending
	}
}

// SyncReconcile reconciles a DIVERGED local branch into LINEAR history using the
// pure field-aware merge engine. It reads the three-way state (base = merge-base,
// ours = local head, theirs = remote head) from Dolt, runs the engine, and — when
// every field resolves — adopts the remote head as the new base and replays the
// merged result as one forward commit, so the log reads as a single continuous
// stream and the subsequent push always fast-forwards. When a free-text field
// diverged on both sides it commits nothing, leaves the local branch untouched,
// and returns the prose conflicts for the agent surface.
//
// [LAW:effects-at-boundaries] This method owns the effects (read/reset/commit);
// the merge DECISION is the pure engine. The reconciling machine knows only its
// own workspace id, so all three exports carry it — ThreeWay then sees equal
// workspace ids on both sides and its deterministic value tiebreak governs. That
// is exactly right here: the linear-history protocol guarantees the remote head
// is a single shared pointer, so at most one machine is diverged against it at a
// time and each (base,ours,theirs) triple is reconciled by exactly one machine —
// cross-machine tiebreak symmetry is not needed, only on-machine determinism,
// which the engine provides regardless of the ids passed.
// [LAW:no-ambient-temporal-coupling] The three commit anchors are captured ONCE,
// before any branch movement, so the retryable read+replay always starts from
// fixed hashes no matter where a retried attempt left the working branch.
//
// CONSTRAINT (embedded Dolt one-RW-engine-per-path): this runs INLINE/foreground
// on the caller's own engine after its command engine closed — never a background
// worker.
func (s *Store) SyncReconcile(ctx context.Context, remote string, branch string) (SyncReconcileResult, error) {
	return s.reconcile(ctx, remote, branch, autonomousSettle)
}

// SyncReconcileResolved is the agent-resolved finalize of a prose-pending
// divergence. It re-derives the SAME three-way state SyncReconcile read — the
// prose-pending state is never snapshot-persisted, so the live refs are the one
// source of truth ([LAW:one-source-of-truth]) — and replays the merge with the
// agent's merged prose spliced in, as the SAME single forward commit on the remote
// head. Resolutions that no longer match the live divergence do not commit; the
// result comes back SyncReconcileProsePending with the CURRENT conflicts for the
// agent to re-merge. [LAW:no-silent-failure]
//
// It shares SyncReconcile's safe-replay verbatim: the merged commit is built on a
// unique scratch branch and the data branch advances with one atomic reset, so an
// interrupted finalize can never orphan the clone's local work.
func (s *Store) SyncReconcileResolved(ctx context.Context, remote string, branch string, resolutions []merge.ProseResolution) (SyncReconcileResult, error) {
	return s.reconcile(ctx, remote, branch, resolvedSettle(resolutions))
}

// reconcilePlan is everything a reconcile decides from, captured ONCE under the
// commit lock before any branch moves: the freshness span, whether the branch is
// diverged at all, and — only when diverged — the fixed commit anchors and the
// shared-history discriminant. Every reconcile variant (the field-aware three-way
// and the unrelated take-one) reads its decision from this one capture, so the two
// can never disagree on what "the divergence" is. [LAW:one-source-of-truth]
// [LAW:no-ambient-temporal-coupling] the anchors are hashes fixed before any move,
// so a retried attempt always starts from the same triple.
type reconcilePlan struct {
	diverged   bool
	ahead      int64
	behind     int64
	dataBranch string
	localHead  string
	remoteHead string
	base       mergeBaseResult
}

// captureReconcilePlan reads the reconcile decision state under the already-held
// commit lock. When the branch is not diverged it returns early with diverged=false
// and no anchors (nothing to reconcile); when diverged it captures the data branch,
// both heads, and the merge-base in one pass. It is the single reader of this state,
// so the three-way reconcile and the take-one resolver share one definition of the
// divergence rather than each re-deriving it. [LAW:single-enforcer]
func (s *Store) captureReconcilePlan(ctx context.Context, remote, branch string) (reconcilePlan, error) {
	var plan reconcilePlan
	fresh, err := s.SyncFreshness(ctx, remote, branch)
	if err != nil {
		return reconcilePlan{}, err
	}
	plan.ahead, plan.behind = fresh.Ahead, fresh.Behind
	if fresh.State() != SyncDiverged {
		// Only a divergence needs a reconcile; every other state is the receive/push
		// side's job. [LAW:dataflow-not-control-flow] the freshness value selects the
		// outcome; a re-run that finds the divergence already resolved lands here.
		return plan, nil
	}
	plan.diverged = true
	trackingRef := fmt.Sprintf("remotes/%s/%s", remote, branch)
	plan.dataBranch, err = activeBranch(ctx, s.db)
	if err != nil {
		return reconcilePlan{}, err
	}
	plan.localHead, err = readDoltHead(ctx, s.db)
	if err != nil {
		return reconcilePlan{}, fmt.Errorf("read local head: %w", err)
	}
	plan.remoteHead, err = commitHashOfRef(ctx, s.db, trackingRef)
	if err != nil {
		return reconcilePlan{}, err
	}
	plan.base, err = mergeBase(ctx, s.db, plan.localHead, trackingRef)
	if err != nil {
		return reconcilePlan{}, err
	}
	return plan, nil
}

func (s *Store) reconcile(ctx context.Context, remote string, branch string, settle settleFn) (SyncReconcileResult, error) {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return SyncReconcileResult{}, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return SyncReconcileResult{}, err
	}

	var result SyncReconcileResult
	err = s.withCommitLock(ctx, func(ctx context.Context) error {
		plan, err := s.captureReconcilePlan(ctx, trimmedRemote, trimmedBranch)
		if err != nil {
			return err
		}
		result.Ahead, result.Behind = plan.ahead, plan.behind
		if !plan.diverged {
			result.State = SyncReconcileNotDiverged
			return nil
		}
		baseCommit, shared := plan.base.shared()
		result.LocalHead, result.RemoteHead, result.BaseCommit = plan.localHead, plan.remoteHead, baseCommit

		if !shared {
			// Unrelated histories: the local branch and the remote-tracking ref share no
			// commit, so there is no base for a three-way merge. Classify it as a
			// first-class state and stop HERE — before the schema guard, the scratch
			// sweep, the snapshot, and every reset — so the base-assuming export path is
			// unreachable. Only read-only queries have run, so both stores are untouched:
			// no partial write. [LAW:no-silent-failure] an unmergeable divergence is
			// surfaced as its own state, never crashed through an absent merge-base.
			// [LAW:dataflow-not-control-flow] the merge-base's shared discriminant selects
			// the outcome; the epic's take-one/union resolutions flow through the same
			// anchors via SyncResolveUnrelated, never a second reconcile engine.
			//
			// Read the both-sides inventory off the two anchors while still under the
			// commit lock — pure AS OF reads that move no branch, so the no-write
			// invariant above holds — so the surface can enumerate what each side holds
			// without a second, unlocked query where the heads could shift.
			// [LAW:no-ambient-temporal-coupling]
			inventory, err := s.unrelatedInventory(ctx, plan.localHead, plan.remoteHead)
			if err != nil {
				return err
			}
			result.State = SyncReconcileUnrelated
			result.Unrelated = inventory
			return nil
		}

		// [LAW:single-enforcer] Refuse BEFORE any scratch branch, snapshot, or write
		// when the remote head is at a schema this binary cannot produce. Adopting an
		// ahead remote head here would lift to a no-op (goose sees nothing pending)
		// and replaceFromExport would then write only this binary's older columns —
		// authoring a replay commit BELOW the remote head's schema and dropping every
		// field the newer schema added. That regression IS the 2026-07-08 incident;
		// the guard reads the remote head's version as data and blocks it. Reusing the
		// remoteHead anchor already captured means no second read and no window for a
		// concurrent fetch to shift the decision. [LAW:no-ambient-temporal-coupling]
		if err := s.guardCommitSchemaAhead(ctx, trimmedRemote, trimmedBranch, plan.remoteHead); err != nil {
			return err
		}

		// Sweep any scratch branches a previously-killed reconcile abandoned, then
		// derive this run's own unique scratch name. The commit lock guarantees no
		// other reconcile is live, so every existing scratch branch is an orphan.
		s.sweepStaleReconcileScratch(ctx)
		scratchBranch := reconcileScratchName()

		// One snapshot guard for the whole reconcile, created here so it survives
		// across GC-contention retries: guard.ensure() takes the snapshot on first
		// call and caches it, so a retried attempt reuses the single recovery point
		// of localHead instead of copying the same state again.
		// [LAW:effects-at-boundaries] one owner of the snapshot effect.
		guard := newSnapshotGuard(s.doltRootDir, migrationSnapshotsDir(s.doltRootDir), formatReconcileSnapshotLabel(time.Now()))
		return retryTransientGCContention(ctx, func(ctx context.Context) error {
			return s.reconcileFromAnchors(ctx, &result, settle, guard, plan.dataBranch, scratchBranch, plan.localHead, plan.remoteHead, baseCommit)
		}, s.reconnect, transientRetryDelay, waitWithContext)
	})
	if err != nil {
		return SyncReconcileResult{}, err
	}
	return result, nil
}

// reconcileFromAnchors reads the three exports at fixed commit hashes, runs the
// engine, and — when settled — builds the merged commit on a scratch branch, then
// advances the data branch to it with one atomic reset. ALL intermediate branch
// movement (the reset-based reads, the merged commit) happens on the scratch
// branch, so the data branch never leaves localHead until the merged commit is
// fully built; the data branch then moves exactly once, atomically, to a finished
// commit. An interruption anywhere before that single reset leaves the data branch
// (and the local work on it) untouched at localHead. [LAW:no-silent-failure] local
// commits are never orphaned by a partial reconcile. It is idempotent: a retry
// re-creates the scratch branch from the same fixed anchors and re-derives the
// same result.
func (s *Store) reconcileFromAnchors(ctx context.Context, result *SyncReconcileResult, settle settleFn, guard *snapshotGuard, dataBranch, scratchBranch, localHead, remoteHead, baseCommit string) error {
	return s.runOnReconcileScratch(ctx, dataBranch, scratchBranch, localHead, func() error {
		ours, err := s.exportAtCommit(ctx, localHead)
		if err != nil {
			return err
		}
		theirs, err := s.exportAtCommit(ctx, remoteHead)
		if err != nil {
			return err
		}
		base, err := s.exportAtCommit(ctx, baseCommit)
		if err != nil {
			return err
		}

		merged := merge.ThreeWay(base, ours, theirs)
		export, pending := settle(merged)
		if len(pending) > 0 {
			// Prose still diverges on both sides: commit nothing. The data branch is still
			// at localHead (only the scratch branch moved), so the clone keeps working on
			// local truth, still diverged; the unresolved divergence IS the durable
			// pending state, re-derivable from the refs rather than a snapshot that can
			// drift. [LAW:one-source-of-truth] Hand the prose conflicts to the agent
			// surface. [LAW:no-silent-failure] never auto-committed by picking a side. The
			// resolved finalize reaches here only when the agent's resolutions no longer
			// match the live divergence, so this same path re-surfaces the CURRENT state.
			result.State = SyncReconcileProsePending
			result.Pending = pending
			return nil
		}

		if err := s.commitReplayAndAdvance(ctx, guard, dataBranch, remoteHead, reconcileCommitMessage, export); err != nil {
			return err
		}
		result.State = SyncReconcileLinearized
		result.Pending = nil
		return nil
	})
}

// runOnReconcileScratch force-creates this run's unique scratch branch at localHead,
// runs body on it, and — whatever body returns — returns the session to the data
// branch and drops the scratch branch. Cleanup recovers a failed switch-back by
// rotating the connection; only if THAT also fails is the store left unusable, and
// then the failure is promoted to body's result (when it would not otherwise mask a
// durable error) rather than swallowed. [LAW:no-silent-failure] Every operation body
// runs happens on the scratch branch, so the data branch never leaves localHead until
// body advances it with one atomic reset. [LAW:single-enforcer] the scratch lifecycle
// is written once, shared by the three-way reconcile and the take-one resolver.
func (s *Store) runOnReconcileScratch(ctx context.Context, dataBranch, scratchBranch, localHead string, body func() error) (err error) {
	// -B recreates the branch if a prior retry of this same run left it behind.
	if err := execProcedureDiscard(ctx, s.db, "DOLT_CHECKOUT", "-B", scratchBranch, localHead); err != nil {
		return fmt.Errorf("create reconcile scratch branch: %w", err)
	}
	defer func() {
		if cleanupErr := s.cleanupReconcileScratch(ctx, dataBranch, scratchBranch); cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()
	return body()
}

// commitReplayAndAdvance is the snapshot-guarded safe-replay every mutating reconcile
// shares: on the scratch branch it adopts remoteHead as the base, LIFTS it to this
// binary's schema, replays export as one forward commit, snapshots the pre-mutation
// data branch, then advances the data branch to that finished commit with one atomic
// reset. The caller has already computed export — a field-aware three-way merge or a
// wholesale take-one — so this owns only the replay, identically for both.
// [LAW:single-enforcer] the safety-critical replay is written once.
//
// The lift is required when remoteHead is older-schema: replaceFromExport writes this
// binary's columns, which the remote commit's table may not yet have; lifting first
// (reusing the migration chain) lands the schema DDL and the replayed data in the SAME
// commit — one committed unit or nothing, never a half-migrated intermediate. The data
// branch is untouched until the single atomic reset. [LAW:no-ambient-temporal-coupling]
// no observer sees a half-built state; it lives only on the scratch branch.
func (s *Store) commitReplayAndAdvance(ctx context.Context, guard *snapshotGuard, dataBranch, remoteHead, message string, export model.Export) error {
	if err := s.resetAndLift(ctx, remoteHead); err != nil {
		return fmt.Errorf("adopt remote head %q on scratch: %w", remoteHead, err)
	}
	if err := s.replaceFromExport(ctx, export, message); err != nil {
		return err
	}
	replayedCommit, err := readDoltHead(ctx, s.db)
	if err != nil {
		return fmt.Errorf("read replayed commit: %w", err)
	}

	// Snapshot-first: capture the pre-mutation filesystem state so this automatic
	// data-branch advance is reversible via `lit snapshots restore`. The data branch
	// is still at its pre-reconcile head at this point (only the scratch branch
	// moved), so the snapshot preserves exactly the clone's pre-reconcile local truth.
	// [LAW:no-silent-failure] a failed snapshot aborts BEFORE the data branch moves,
	// rather than performing an irreversible automatic mutation with no recovery point.
	// The guard is owned by the caller and shared across GC-contention retries, so
	// ensure() takes exactly one snapshot no matter how many attempts run.
	if _, err := guard.ensure(); err != nil {
		return fmt.Errorf("snapshot before reconcile: %w", err)
	}

	// Advance the data branch to the finished commit with one atomic reset. This is
	// the only mutation of the data branch; before it, the data branch is at its
	// pre-reconcile head, after it, at the complete replayed commit — never an
	// in-between. [LAW:one-source-of-truth] one authoritative ordering; no per-machine
	// merge-commit DAG — linear history that fast-forward pushes.
	if err := execProcedureDiscard(ctx, s.db, "DOLT_CHECKOUT", dataBranch); err != nil {
		return fmt.Errorf("return to data branch %q: %w", dataBranch, err)
	}
	if _, err := callIntProcedure(ctx, s.db, "DOLT_RESET", "--hard", replayedCommit); err != nil {
		return fmt.Errorf("advance %q to replayed commit: %w", dataBranch, err)
	}
	// The data branch now durably holds the result; roll the recovery snapshots off to
	// the retention budget. IsReconcileSnapshotName is disjoint from the
	// migration/downgrade classifiers, so this only collects reconcile snapshots.
	// [LAW:single-enforcer] reconcile owns its own snapshot budget.
	//
	// The prune runs AFTER the durable advance, so a prune failure must NOT be promoted
	// to fail the reconcile — the replay already committed, and returning an error here
	// would report a successful reconcile as failed (and a retry would find it already
	// resolved). A leftover un-pruned snapshot is inert, exactly like the leftover
	// scratch branch cleanupReconcileScratch tolerates, so the failure is surfaced to
	// stderr but not promoted. [LAW:no-silent-failure] loud, but not a false failure
	// over a durable success.
	if err := dbsnapshot.PruneMatching(migrationSnapshotsDir(s.doltRootDir), reconcileSnapshotRetention, IsReconcileSnapshotName); err != nil {
		fmt.Fprintf(os.Stderr, "lit: reconcile could not prune old recovery snapshots (replay already committed): %v\n", err)
	}
	return nil
}

// cleanupReconcileScratch returns the session to the data branch and deletes the
// scratch branch. The data-branch pointer already holds the durable result before
// this runs, so the one thing cleanup must still guarantee is that the session is
// not left on the scratch branch — otherwise a later use of this store would
// silently read/write the wrong branch. If the switch back fails, the connection
// IS stranded on scratch, so it is rotated: a fresh connection always opens on the
// data branch. Only if that rotation ALSO fails is the store genuinely unusable —
// an unrecoverable state the caller promotes to the reconcile's error rather than
// tolerating. A failed scratch-branch delete is recoverable (the next reconcile
// force-recreates the name and it is never pushed), so it is surfaced but not
// promoted. [LAW:no-silent-failure] recoverable failures recover loudly;
// unrecoverable ones fail the operation.
func (s *Store) cleanupReconcileScratch(ctx context.Context, dataBranch, scratchBranch string) error {
	if err := execProcedureDiscard(ctx, s.db, "DOLT_CHECKOUT", dataBranch); err != nil {
		fmt.Fprintf(os.Stderr, "lit: reconcile could not return to data branch %q (%v); rotating connection to recover\n", dataBranch, err)
		if reconnectErr := s.reconnect(); reconnectErr != nil {
			return fmt.Errorf("reconcile left the store on the scratch branch and could not recover: checkout %q failed (%v); connection rotation failed: %w", dataBranch, err, reconnectErr)
		}
		// A fresh connection opens on the default branch, but do not rely on that —
		// check out the data branch explicitly on it and treat a failure as the
		// unrecoverable case, so the store is provably on the data branch (or the
		// reconcile errors) rather than assumed to be. The leftover scratch branch is
		// harmless: the next reconcile's startup sweep drops it and it is never pushed.
		if recoverErr := execProcedureDiscard(ctx, s.db, "DOLT_CHECKOUT", dataBranch); recoverErr != nil {
			return fmt.Errorf("reconcile could not restore the data branch %q after rotating the connection: %w", dataBranch, recoverErr)
		}
		return nil
	}
	if err := execProcedureDiscard(ctx, s.db, "DOLT_BRANCH", "-D", scratchBranch); err != nil {
		fmt.Fprintf(os.Stderr, "lit: reconcile cleanup could not delete scratch branch %q: %v\n", scratchBranch, err)
	}
	return nil
}

// sweepStaleReconcileScratch deletes scratch branches abandoned by a previously
// killed reconcile. The commit lock guarantees no other reconcile is live, so
// every branch under the scratch prefix is an orphan — deleting them keeps the
// unique-per-run names from accumulating. Best-effort: a sweep failure is surfaced
// but never fails the reconcile, since a leftover scratch branch is inert (never
// the data branch, never pushed). [LAW:no-silent-failure]
func (s *Store) sweepStaleReconcileScratch(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM dolt_branches WHERE name LIKE ?`, reconcileScratchPrefix+"-%")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: reconcile could not list stale scratch branches: %v\n", err)
		return
	}
	var names []string
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			fmt.Fprintf(os.Stderr, "lit: reconcile could not scan stale scratch branch: %v\n", scanErr)
			rows.Close()
			return
		}
		names = append(names, name)
	}
	if iterErr := rows.Err(); iterErr != nil {
		fmt.Fprintf(os.Stderr, "lit: reconcile could not iterate stale scratch branches: %v\n", iterErr)
	}
	rows.Close()
	for _, name := range names {
		if delErr := execProcedureDiscard(ctx, s.db, "DOLT_BRANCH", "-D", name); delErr != nil {
			fmt.Fprintf(os.Stderr, "lit: reconcile could not delete stale scratch branch %q: %v\n", name, delErr)
		}
	}
}

// resetAndLift hard-resets the (scratch) branch to a commit and then lifts the
// working set to this binary's registry schema. The reset adopts that commit's
// schema — which may be OLDER than the binary's when the commit was written by a
// prior lit version (the multi-machine schema-skew the reconcile must be total
// over) — so the lift replays the missing migrations' DDL, filling new columns
// with their NULL/default, before any read or write against the working set. On
// a commit already at registry max (the common case — the local head is always
// current) the lift is a no-op. It is safe because the caller runs it only on
// the throwaway scratch branch, never the data branch.
// [LAW:types-are-the-program] the reconcile's input domain includes "an anchor
// at an older schema version"; resetAndLift is what makes that state legal to
// read and write instead of a raw backend error. [LAW:one-source-of-truth] the
// lift reuses the migration chain, not a second schema-adaptation path.
func (s *Store) resetAndLift(ctx context.Context, commit string) error {
	if _, err := callIntProcedure(ctx, s.db, "DOLT_RESET", "--hard", commit); err != nil {
		return fmt.Errorf("reset scratch to %q: %w", commit, err)
	}
	if err := s.liftWorkingSetToRegistry(ctx); err != nil {
		return fmt.Errorf("lift %q to current schema: %w", commit, err)
	}
	return nil
}

// exportAtCommit resets the (scratch) branch to a commit, lifts it to the
// current schema, and exports it. Reading at a revision this way reuses the one
// canonical export path; it is safe because the caller runs it only on the
// scratch branch, never the data branch. [LAW:single-enforcer]
func (s *Store) exportAtCommit(ctx context.Context, commit string) (model.Export, error) {
	if err := s.resetAndLift(ctx, commit); err != nil {
		return model.Export{}, fmt.Errorf("read export at %q: %w", commit, err)
	}
	return s.Export(ctx)
}

// activeBranch reads the session's current branch — the live data branch the
// reconcile must restore and advance.
func activeBranch(ctx context.Context, db *sql.DB) (string, error) {
	var branch string
	if err := db.QueryRowContext(ctx, `SELECT active_branch()`).Scan(&branch); err != nil {
		return "", fmt.Errorf("read active branch: %w", err)
	}
	return branch, nil
}

// commitHashOfRef returns the head commit hash of a ref (e.g. a remote-tracking
// ref). dolt_log(ref) lists commits reachable from ref newest-first, so LIMIT 1
// is its head. The ref is bound, not interpolated. [LAW:single-enforcer]
func commitHashOfRef(ctx context.Context, db *sql.DB, ref string) (string, error) {
	var head string
	if err := db.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log(?) LIMIT 1`, ref).Scan(&head); err != nil {
		return "", fmt.Errorf("read head of %q: %w", ref, err)
	}
	return head, nil
}

// mergeBaseResult is the outcome of a merge-base query: either the two refs share
// history — commit is their most-recent common ancestor — or they do not, and
// there is no base for a three-way merge. DOLT_MERGE_BASE reports the no-ancestor
// case as an empty result set (and some backends as an empty/NULL scalar); this
// type lifts that absence into an explicit discriminator so "no common ancestor"
// can never be mistaken for a commit hash and driven into the base-assuming export
// path, which resets to it and fails obscurely. [LAW:types-are-the-program] the
// absent base is unrepresentable as a commit; a caller reaches the hash only
// through shared(), which reports its absence.
type mergeBaseResult struct {
	commit  string
	hasBase bool
}

// shared reports the common-ancestor commit and whether one exists. When ok is
// false the two refs have unrelated histories and commit is meaningless.
func (r mergeBaseResult) shared() (commit string, ok bool) {
	return r.commit, r.hasBase
}

// mergeBase returns the merge-base of two refs — the most recent commit reachable
// from both, i.e. the three-way merge's base — or the unrelated-histories state
// when they share none. The refs are bound, not interpolated.
func mergeBase(ctx context.Context, db *sql.DB, ref1, ref2 string) (mergeBaseResult, error) {
	var base sql.NullString
	err := db.QueryRowContext(ctx, `SELECT DOLT_MERGE_BASE(?, ?)`, ref1, ref2).Scan(&base)
	if errors.Is(err, sql.ErrNoRows) {
		// DOLT_MERGE_BASE returns NO ROWS for refs with no common ancestor. That
		// absence is a real domain state — unrelated histories — not a query failure,
		// so it is carried as shared=false rather than surfaced as an obscure
		// "sql: no rows in result set". [LAW:no-defensive-null-guards] the absence is
		// matched as a value at this backend boundary, not papered over downstream.
		return mergeBaseResult{}, nil
	}
	if err != nil {
		return mergeBaseResult{}, fmt.Errorf("merge-base of %q and %q: %w", ref1, ref2, err)
	}
	// Belt-and-suspenders across backend versions: a NULL or empty scalar spells the
	// same "no ancestor" as an empty result set. [LAW:no-defensive-null-guards] the
	// absence is a real value at the boundary, handled identically to the no-row form.
	if trimmed := strings.TrimSpace(base.String); base.Valid && trimmed != "" {
		return mergeBaseResult{commit: trimmed, hasBase: true}, nil
	}
	return mergeBaseResult{}, nil
}
