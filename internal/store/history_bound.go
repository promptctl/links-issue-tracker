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
// commit count: a push that leaves the remote head at height ReRootAt or
// above re-roots it, keeping the Keep commits of the head's first-parent line.
// It is a count, not an age, because the store grows linearly with its
// commits while the rate of commits swings month to month (the decision on
// links-storage-3k6u). Keep is below ReRootAt, so a re-root always drops
// something.
type historyBound struct {
	ReRootAt, Keep uint64
}

// DefaultHistoryBound is the bound every store opens with. It holds this
// repository's store between 35.8 MB and 64.7 MB, measured on a copy of it
// (links-storage-3k6u.8ka). A package variable so a regression test can
// shrink it to drive a push past it in a few writes.
var DefaultHistoryBound = historyBound{ReRootAt: 2000, Keep: 1000}

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
//  1. it re-roots a scratch branch at that head, keeping s.bound.Keep commits
//     of the head's first-parent line (reRootScratch);
//  2. it force-pushes the scratch to the remote's branch under a lease that
//     names the head (publishReRoot);
//  3. it runs the remote's GC, which replaces the remote's table files with
//     the ones the re-rooted head reaches while the remote carries it
//     (collectRemote);
//  4. it fetches, and follows whatever re-root the remote now carries. A held
//     lease means the remote carries this one. A stale lease is only the last
//     transport attempt's verdict, and an earlier attempt may have landed
//     before a peer built on it (storage.LeaseStale), so the fetch is what
//     says whether the remote carries the re-root; a remote a peer pushed to
//     in between carries none, and the branch stays where it was;
//  5. once the branch has followed, it reclaims the history below the cut,
//     as every follow does (collectBelowTheCut).
//
// A branch that did not follow is left on the history it had, and the next
// push past the bound tries again; that is only sound while the remote still
// shares that history, so a branch left unrelated to the remote is the
// bound's error. Each step takes its own locks: the re-root and the follow
// rewrite history with the engine closed, and the fetch takes the commit lock
// itself, so it cannot run inside the lease push's mutation.
//
// The bound is recorded for HistoryBounds whatever happened to it, and its
// error is its own: the push before it landed. [LAW:nothing-unseen]
func (s *Store) boundHistory(ctx context.Context, remote, branch string) storage.HistoryBound {
	b := storage.HistoryBound{Remote: remote, Branch: branch, ReRootAt: s.bound.ReRootAt, Keep: s.bound.Keep}
	start := time.Now()
	b.Err = s.keepHistoryBound(ctx, &b)
	b.Elapsed = time.Since(start)
	s.bounds = append(s.bounds, b)
	return b
}

// SyncKeepHistoryBound runs boundHistory after a push that landed, and joins
// its account to the push's maintenance. See the storage.Syncer contract.
func (s *Store) SyncKeepHistoryBound(ctx context.Context, remote, branch string, pushed storage.SyncPushResult) storage.SyncPushResult {
	bound := s.boundHistory(ctx, strings.TrimSpace(remote), strings.TrimSpace(branch))
	pushed.Maintenance = joinMaintenance(pushed.Maintenance, historyBoundReport(bound))
	return pushed
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
	b.ReRooted, err = s.reRootScratch(ctx, b.Branch, b.Head, b.Keep)
	if err == nil {
		b.Lease, err = s.publishReRoot(ctx, b.Remote, b.Branch, b.Head)
	}
	// The scratch lives only as long as the re-root and its push, and goes
	// however they ended. A re-root is deterministic, so the next push past
	// the bound writes the same commit again, while a scratch left behind
	// would keep the whole history below the cut reachable through every GC
	// until then. One a killed bound left goes with the next collection below
	// a cut (collectBelowTheCut).
	sweepErr := s.runSyncMutation(ctx, func(ctx context.Context) error {
		return s.deleteBranchesUnder(ctx, reRootScratchPrefix)
	})
	if err = errors.Join(err, sweepErr); err != nil {
		return err
	}
	// [LAW:dataflow-not-control-flow] the remote's GC, the fetch and the
	// follow run whatever the lease said. The remote's root decides whether
	// its GC replaces anything: only while the remote carries this re-root and
	// nothing on top of it, a held lease and a stale verdict over an attempt
	// that landed alike. Its error is the bound's, and stops nothing after it:
	// this branch follows the re-root it published whatever became of the
	// remote's table files.
	var remoteGCErr error
	b.RemoteGC, remoteGCErr = s.collectRemote(ctx, b.Remote, b.Branch, b.ReRooted.Head)
	if remoteGCErr != nil {
		remoteGCErr = fmt.Errorf("collect %s: %w", trackingRef, remoteGCErr)
	}
	return errors.Join(s.followPublishedReRoot(ctx, b, trackingRef), remoteGCErr)
}

// followPublishedReRoot is publishBound's steps after the remote's GC: fetch,
// and follow whatever re-root the remote carries, collecting below the cut
// as every follow does.
func (s *Store) followPublishedReRoot(ctx context.Context, b *storage.HistoryBound, trackingRef string) error {
	if err := s.SyncFetch(ctx, b.Remote, false); err != nil {
		return fmt.Errorf("fetch to learn whether %s carries the re-root: %w", trackingRef, err)
	}
	followed, err := s.followRemoteReRoot(ctx, b.Remote, b.Branch, storage.SyncEntryBound)
	b.Collection = followed.Collection
	// The follow's record has a head exactly when the branch moved.
	if err != nil || followed.Head != "" {
		return err
	}
	// The follow declines a re-root it cannot recognize as this branch's, and
	// at the other entry points the reconcile then reports the divergence. No
	// reconcile runs here, so the bound reports it: a remote carrying a re-root
	// the branch did not follow, this push's own among them, leaves the branch
	// on history unrelated to it. [LAW:no-silent-failure]
	_, stranded, err := s.unrelatedToRemote(ctx, b.Remote, b.Branch)
	if err != nil {
		return fmt.Errorf("read whether the branch still shares history with %s: %w", trackingRef, err)
	}
	if stranded {
		return fmt.Errorf("%s carries a re-root the branch did not follow, so the branch is on history unrelated to it", trackingRef)
	}
	return nil
}

// reRootScratch writes the re-root of head onto branch's scratch branch, cut
// where head's first-parent line keeps keep commits.
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
			return fmt.Errorf("re-root %s at %s: %w", head, cut, err)
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
// only while that branch still names head.
func (s *Store) publishReRoot(ctx context.Context, remote, branch, head string) (storage.LeaseOutcome, error) {
	var leased storage.LeasedPush
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
		var err error
		leased, err = s.pushLeasedWithinLock(ctx, remote, reRootScratchPrefix+branch, branch, head)
		return err
	})
	return leased.Outcome, err
}

// collectBelowTheCut reclaims the history below the cut once the sync branch
// has followed a re-root: under one commit lock, it moves remote's fork
// points onto the history their tracking refs now name, drops the migration
// checkpoints, the reconcile scratch branches and any re-root scratch a
// killed bound left, every one of which names a commit from before the
// re-root, and runs the full GC. Every follow runs after a fetch moved the
// tracking ref onto the re-rooted head; while that ref still named the old
// head, the same GC freed nothing (links-storage-3k6u.ge9). A ref it could
// not drop still pins that history, so it stops the pass before the GC.
//
// The store is measured around the pass, outside the lock, as SyncCompact
// measures it: the two readings are an account of what happened and never an
// input to it, so a reading that fails joins the collection's error and stops
// nothing. The collection's error is its own (storage.Collection.Err).
func (s *Store) collectBelowTheCut(ctx context.Context, remote string) storage.Collection {
	var c storage.Collection
	var beforeErr, afterErr error
	c.BytesBefore, beforeErr = storeBytes(s.doltRootDir)
	var sweep refSweepOutcome
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
		forkPoints, err := s.recordForkPoints(ctx, remote)
		s.forkPoints = append(s.forkPoints, forkPoints...)
		if err != nil {
			return err
		}
		for _, prefix := range []string{migrationCheckpointPrefix, migrationDriftRepairCheckpointPrefix} {
			if err := s.PruneCheckpoints(ctx, prefix, 0); err != nil {
				return err
			}
		}
		for _, prefix := range []string{reconcileScratchPrefix + "-", reRootScratchPrefix} {
			if err := s.deleteBranchesUnder(ctx, prefix); err != nil {
				return err
			}
		}
		attempt, swept, err := s.compactWithinLock(ctx, GCFull)
		c.Ran = attempt.Ran
		sweep = sweep.plus(swept)
		return err
	})
	c.BytesAfter, afterErr = storeBytes(s.doltRootDir)
	c.Sweep = sweep.Report()
	c.Err = errors.Join(err, beforeErr, afterErr)
	return c
}

// collectionReport is a collection in the maintenance vocabulary: the store's
// size across the GC when it ran, the GC's sweep line, and where the pass
// stopped. A collection that did nothing and failed nothing says nothing.
func collectionReport(c storage.Collection) string {
	var parts []string
	if c.Ran {
		parts = append(parts, fmt.Sprintf("store %s -> %s", humanBytes(c.BytesBefore), humanBytes(c.BytesAfter)))
	}
	if c.Sweep != "" {
		parts = append(parts, c.Sweep)
	}
	if c.Err != nil {
		parts = append(parts, fmt.Sprintf("history below the cut not collected: %v", c.Err))
	}
	return joinMaintenance(parts...)
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
	if g := b.RemoteGC; g.Outcome != "" {
		parts = append(parts, fmt.Sprintf("remote GC %s: %s of table files, %s collected, %d other branches dropped", g.Outcome, humanBytes(g.BytesBefore), humanBytes(g.CollectedBytes), len(g.DroppedBranches)))
	}
	parts = append(parts, collectionReport(b.Collection))
	if b.Err != nil {
		parts = append(parts, fmt.Sprintf("history bound not kept: %v", b.Err))
	}
	return joinMaintenance(parts...)
}
