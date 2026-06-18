package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

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
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return SyncReconcileResult{}, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return SyncReconcileResult{}, err
	}
	trackingRef := fmt.Sprintf("remotes/%s/%s", trimmedRemote, trimmedBranch)

	var result SyncReconcileResult
	err = s.withCommitLock(ctx, func(ctx context.Context) error {
		fresh, err := s.SyncFreshness(ctx, trimmedRemote, trimmedBranch)
		if err != nil {
			return err
		}
		result.Ahead, result.Behind = fresh.Ahead, fresh.Behind
		if fresh.State() != SyncDiverged {
			// Only a divergence needs the three-way merge; every other state is the
			// receive/push side's job. [LAW:dataflow-not-control-flow] The freshness
			// value selects the outcome; this is idempotent under a re-run that finds
			// the divergence already resolved.
			result.State = SyncReconcileNotDiverged
			return nil
		}
		dataBranch, err := activeBranch(ctx, s.db)
		if err != nil {
			return err
		}
		localHead, err := readDoltHead(ctx, s.db)
		if err != nil {
			return fmt.Errorf("read local head: %w", err)
		}
		remoteHead, err := commitHashOfRef(ctx, s.db, trackingRef)
		if err != nil {
			return err
		}
		baseCommit, err := mergeBase(ctx, s.db, localHead, trackingRef)
		if err != nil {
			return err
		}
		result.LocalHead, result.RemoteHead, result.BaseCommit = localHead, remoteHead, baseCommit

		// Sweep any scratch branches a previously-killed reconcile abandoned, then
		// derive this run's own unique scratch name. The commit lock guarantees no
		// other reconcile is live, so every existing scratch branch is an orphan.
		s.sweepStaleReconcileScratch(ctx)
		scratchBranch := reconcileScratchName()

		return retryTransientGCContention(ctx, func(ctx context.Context) error {
			return s.reconcileFromAnchors(ctx, &result, dataBranch, scratchBranch, localHead, remoteHead, baseCommit)
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
func (s *Store) reconcileFromAnchors(ctx context.Context, result *SyncReconcileResult, dataBranch, scratchBranch, localHead, remoteHead, baseCommit string) (err error) {
	// Force-create this run's unique scratch branch at the local head and switch to
	// it; -B recreates it if a prior retry of this same run left it behind.
	if err := execProcedureDiscard(ctx, s.db, "DOLT_CHECKOUT", "-B", scratchBranch, localHead); err != nil {
		return fmt.Errorf("create reconcile scratch branch: %w", err)
	}
	// Whatever happens, return the session to the data branch and drop this run's
	// scratch branch. Cleanup recovers a failed switch-back by rotating the
	// connection; only if THAT also fails is the store left unusable, and then the
	// failure is promoted to the reconcile's result (when it would not otherwise
	// mask a durable error) rather than swallowed. [LAW:no-silent-failure]
	defer func() {
		if cleanupErr := s.cleanupReconcileScratch(ctx, dataBranch, scratchBranch); cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()

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
	export, ok := merged.Settled()
	if !ok {
		// Prose diverged on both sides: commit nothing. The data branch is still at
		// localHead (only the scratch branch moved), so the clone keeps working on
		// local truth, still diverged; the unresolved divergence IS the durable
		// pending state, re-derivable from the refs rather than a snapshot that can
		// drift. [LAW:one-source-of-truth] Hand the prose conflicts to the agent
		// surface. [LAW:no-silent-failure] never auto-committed by picking a side.
		result.State = SyncReconcileProsePending
		result.Pending = merged.Pending
		return nil
	}

	// Build the merged commit on the scratch branch: adopt the remote head as its
	// base and replay the merged result as one forward commit. The data branch is
	// untouched throughout.
	if _, err := callIntProcedure(ctx, s.db, "DOLT_RESET", "--hard", remoteHead); err != nil {
		return fmt.Errorf("adopt remote head %q on scratch: %w", remoteHead, err)
	}
	if err := s.replaceFromExport(ctx, export, reconcileCommitMessage); err != nil {
		return err
	}
	mergedCommit, err := readDoltHead(ctx, s.db)
	if err != nil {
		return fmt.Errorf("read merged commit: %w", err)
	}

	// Advance the data branch to the finished merged commit with one atomic reset.
	// This is the only mutation of the data branch; before it, the data branch is
	// at localHead, after it, at the complete merged commit — never an in-between.
	// [LAW:one-source-of-truth] one authoritative ordering; no per-machine
	// merge-commit DAG, no evidence a merge happened — linear history that
	// fast-forward pushes.
	if err := execProcedureDiscard(ctx, s.db, "DOLT_CHECKOUT", dataBranch); err != nil {
		return fmt.Errorf("return to data branch %q: %w", dataBranch, err)
	}
	if _, err := callIntProcedure(ctx, s.db, "DOLT_RESET", "--hard", mergedCommit); err != nil {
		return fmt.Errorf("advance %q to merged commit: %w", dataBranch, err)
	}
	result.State = SyncReconcileLinearized
	result.Pending = nil
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
		// Rotation recovered: the fresh connection is on the data branch. The leftover
		// scratch branch is harmless — the next reconcile's startup sweep drops it and
		// it is never pushed (only the data branch is).
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

// exportAtCommit hard-resets the (scratch) branch to a commit and exports it.
// Reading at a revision this way reuses the one canonical export path; it is safe
// because the caller runs it only on the scratch branch, never the data branch.
// [LAW:single-enforcer]
func (s *Store) exportAtCommit(ctx context.Context, commit string) (model.Export, error) {
	if _, err := callIntProcedure(ctx, s.db, "DOLT_RESET", "--hard", commit); err != nil {
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

// mergeBase returns the merge-base commit of two refs — the most recent commit
// reachable from both, i.e. the three-way merge's base. The refs are bound, not
// interpolated.
func mergeBase(ctx context.Context, db *sql.DB, ref1, ref2 string) (string, error) {
	var base string
	if err := db.QueryRowContext(ctx, `SELECT DOLT_MERGE_BASE(?, ?)`, ref1, ref2).Scan(&base); err != nil {
		return "", fmt.Errorf("merge-base of %q and %q: %w", ref1, ref2, err)
	}
	return base, nil
}
