package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// historyBound is the bound on the sync branch's reachable history, as a
// commit count: a push that leaves the remote head at height reRootAt or
// above re-roots it, keeping the keep commits of the head's first-parent line.
// It is a count, not an age, because the store grows linearly with its
// commits while the rate of commits swings month to month (the decision on
// links-storage-3k6u). keep is below reRootAt, so a re-root always drops
// something.
type historyBound struct {
	reRootAt, keep uint64
}

// defaultHistoryBound holds this repository's store between 35.8 MB and
// 64.7 MB, measured on a copy of it (links-storage-3k6u.8ka).
var defaultHistoryBound = historyBound{reRootAt: 2000, keep: 1000}

// reRootScratchPrefix names the branch a bound's re-root is written on before
// it is published. The sync branch stays on the history the remote had until
// the remote carries the re-root, and reaches it the way every other checkout
// does, by following it. A branch re-rooted ahead of the remote could not:
// were the lease to find a peer's re-root at a lower cut, the branch would no
// longer hold the commit that root re-commits (recognizeReRootCut), and the
// remote would stay unrelated histories.
const reRootScratchPrefix = "lit-reroot/"

// boundHistory keeps the sync branch's history under s.bound once a push has
// landed on the remote's branch. It reads the height of the remote head the
// push left, which is one row of dolt_log: its commit_order is the commit's
// height. Below the bound that is all it does. Once the bound is reached:
//
//  1. it re-roots a scratch branch at that head, keeping s.bound.keep commits
//     of the head's first-parent line (reRootScratch);
//  2. it force-pushes the scratch to the remote's branch under a lease that
//     names the head (publishReRoot);
//  3. it fetches, and follows whatever re-root the remote now carries. A held
//     lease means the remote carries this one. A stale lease is only the last
//     transport attempt's verdict, and an earlier attempt may have landed
//     before a peer built on it (storage.LeaseStale), so the fetch is what
//     says whether the remote carries the re-root; a remote a peer pushed to
//     in between carries none, and the branch stays where it was;
//  4. once the branch has followed, it drops the local refs that pin the
//     history below the cut and runs the full GC (collectBelowTheCut).
//
// A branch that did not follow is left on the history it had, and the next
// push past the bound tries again. Each step takes its own locks: the re-root
// and the follow rewrite history with the engine closed, and the fetch takes
// the commit lock itself, so it cannot run inside the lease push's mutation.
//
// The bound is recorded for HistoryBounds whatever happened to it, and its
// error is its own: the push before it landed. [LAW:nothing-unseen]
func (s *Store) boundHistory(ctx context.Context, remote, branch string) storage.HistoryBound {
	b := storage.HistoryBound{Remote: remote, Branch: branch, ReRootAt: s.bound.reRootAt, Keep: s.bound.keep}
	b.Err = s.keepHistoryBound(ctx, &b)
	s.bounds = append(s.bounds, b)
	return b
}

// keepHistoryBound runs boundHistory's steps, filling in b as each one lands.
func (s *Store) keepHistoryBound(ctx context.Context, b *storage.HistoryBound) error {
	if _, err := requireSyncArg("branch", b.Branch); err != nil {
		return err
	}
	trackingRef := fmt.Sprintf("remotes/%s/%s", b.Remote, b.Branch)
	if err := s.db.QueryRowContext(ctx, `SELECT commit_hash, commit_order FROM dolt_log(?) LIMIT 1`, trackingRef).Scan(&b.Head, &b.Height); err != nil {
		return fmt.Errorf("read the height of %s: %w", trackingRef, err)
	}
	if !b.Fired() {
		return nil
	}
	return s.publishBound(ctx, b)
}

// publishBound is boundHistory's steps once the bound fired at b.Head.
func (s *Store) publishBound(ctx context.Context, b *storage.HistoryBound) error {
	trackingRef := fmt.Sprintf("remotes/%s/%s", b.Remote, b.Branch)
	var err error
	if b.ReRooted, err = s.reRootScratch(ctx, b.Branch, b.Head, b.Keep); err != nil {
		return err
	}
	if b.Lease, err = s.publishReRoot(ctx, b.Remote, b.Branch, b.Head); err != nil {
		return err
	}
	// [LAW:dataflow-not-control-flow] the fetch and the follow run whatever
	// the lease said: what the remote carries decides whether the branch
	// moves, and the follow is the one way a branch reaches re-rooted history.
	if err := s.SyncFetch(ctx, b.Remote, false); err != nil {
		return fmt.Errorf("fetch to learn whether %s carries the re-root: %w", trackingRef, err)
	}
	followed, err := s.followRemoteReRoot(ctx, b.Remote, b.Branch, storage.SyncEntryBound)
	if err != nil || !followed {
		return err
	}
	return s.collectBelowTheCut(ctx, b)
}

// reRootScratch writes the re-root of head onto branch's scratch branch, cut
// where head's first-parent line keeps keep commits. A re-root refused, for a
// side line across the cut among other things, deletes the scratch again.
func (s *Store) reRootScratch(ctx context.Context, branch, head string, keep uint64) (storage.ReRooted, error) {
	scratch := ref.NewBranchRef(reRootScratchPrefix + branch)
	var reRooted storage.ReRooted
	err := s.withEngineClosed(ctx, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		headCommit, err := readFullCommit(holdCtx, ddb, hash.Parse(head))
		if err != nil {
			return fmt.Errorf("read the remote head %s: %w", head, err)
		}
		cut, err := firstParentAncestor(holdCtx, ddb, headCommit, keep-1)
		if err != nil {
			return err
		}
		if err := ddb.SetHeadToCommit(holdCtx, scratch, headCommit); err != nil {
			return fmt.Errorf("point %s at %s: %w", scratch, head, err)
		}
		start := time.Now()
		reRooted, err = reRootBranch(holdCtx, ddb, scratch, cut)
		reRooted.Elapsed = time.Since(start)
		if err != nil {
			err = fmt.Errorf("re-root %s at %s: %w", head, cut, err)
			return errors.Join(err, ddb.DeleteBranch(holdCtx, scratch, nil))
		}
		return nil
	})
	return reRooted, err
}

// firstParentAncestor is the commit steps first parents below c.
func firstParentAncestor(ctx context.Context, ddb *doltdb.DoltDB, c *doltdb.Commit, steps uint64) (hash.Hash, error) {
	addr, err := c.HashOf()
	if err != nil {
		return hash.Hash{}, err
	}
	for step := range steps {
		parents, err := c.ParentHashes(ctx)
		if err != nil {
			return hash.Hash{}, err
		}
		if len(parents) == 0 {
			return hash.Hash{}, fmt.Errorf("the first-parent line below %s ends after %d commits, short of the %d the bound keeps", addr, step+1, steps+1)
		}
		addr = parents[0]
		if c, err = readFullCommit(ctx, ddb, addr); err != nil {
			return hash.Hash{}, fmt.Errorf("read commit %s: %w", addr, err)
		}
	}
	return addr, nil
}

// publishReRoot force-pushes branch's scratch to the remote's branch, landing
// only while that branch still names head, and deletes the scratch once the
// push reached a verdict. A push that reached none keeps it, so a retry of
// this mutation pushes the same commit again.
func (s *Store) publishReRoot(ctx context.Context, remote, branch, head string) (storage.LeaseOutcome, error) {
	scratch := reRootScratchPrefix + branch
	var leased storage.LeasedPush
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
		var err error
		if leased, err = s.pushLeasedWithinLock(ctx, remote, scratch, branch, head); err != nil {
			return err
		}
		return execProcedureDiscard(ctx, s.db, "DOLT_BRANCH", "-D", scratch)
	})
	return leased.Outcome, err
}

// collectBelowTheCut reclaims the history below the cut once the sync branch
// has followed the re-root: under one commit lock, it moves remote's fork
// points onto the history their tracking refs now name, drops the migration
// checkpoints and the reconcile scratch branches, every one of which names a
// commit from before the re-root, and runs the full GC, measuring the store
// around it. The push has moved the tracking ref by now; while it still named
// the old head, the same GC freed nothing (links-storage-3k6u.ge9).
func (s *Store) collectBelowTheCut(ctx context.Context, b *storage.HistoryBound) error {
	before, err := storeBytes(s.doltRootDir)
	if err != nil {
		return err
	}
	b.BytesBefore = before
	err = s.runSyncMutation(ctx, func(ctx context.Context) error {
		forkPoints, err := s.recordForkPoints(ctx, b.Remote)
		s.forkPoints = append(s.forkPoints, forkPoints...)
		if err != nil {
			return err
		}
		for _, prefix := range []string{migrationCheckpointPrefix, migrationDriftRepairCheckpointPrefix} {
			if err := s.PruneCheckpoints(ctx, prefix, 0); err != nil {
				return err
			}
		}
		s.sweepStaleReconcileScratch(ctx)
		attempt, _, err := s.compactWithinLock(ctx, GCFull)
		b.Collected = attempt.Ran
		return err
	})
	if err != nil {
		return err
	}
	b.BytesAfter, err = storeBytes(s.doltRootDir)
	return err
}

// HistoryBounds is every bound boundHistory kept on this store since it
// opened, oldest first.
func (s *Store) HistoryBounds() []storage.HistoryBound {
	return slices.Clone(s.bounds)
}

// historyBoundReport is the bound's part of a push's maintenance account:
// nothing while the head is under the bound, as an ordinary push says nothing,
// and once the bound fired, what it published and reclaimed, or where it
// stopped.
func historyBoundReport(b storage.HistoryBound) string {
	var parts []string
	if b.Fired() {
		parts = append(parts, fmt.Sprintf("history bound: the remote head reached height %d (bound %d)", b.Height, b.ReRootAt))
	}
	if b.ReRooted.Head != "" {
		parts = append(parts, fmt.Sprintf("re-rooted at %s, keeping %d commits and dropping %d", b.ReRooted.Cut, b.ReRooted.Kept(), b.ReRooted.Dropped))
	}
	if b.Lease != "" {
		parts = append(parts, "lease "+string(b.Lease))
	}
	if b.Collected {
		parts = append(parts, fmt.Sprintf("store %s -> %s", humanBytes(b.BytesBefore), humanBytes(b.BytesAfter)))
	}
	if b.Err != nil {
		parts = append(parts, fmt.Sprintf("history bound not kept: %v", b.Err))
	}
	return strings.Join(parts, "; ")
}
