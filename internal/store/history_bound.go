package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/promptctl/links-issue-tracker/internal/historywindow"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// reRootScratchPrefix names the branch a bound's re-root is written on before
// it is published. The sync branch stays on the history the remote had until
// the remote carries the re-root, and reaches it the way every other checkout
// does, by following it. A branch re-rooted ahead of the remote could not:
// were the lease to find a peer's re-root at a lower cut, the branch would no
// longer hold the commit that root re-commits (recognizeReRootCut), and the
// remote would stay unrelated histories.
const reRootScratchPrefix = "lit-reroot/"

// boundHistory keeps the sync branch's history inside window once a push has
// landed on the remote's branch. It reads the height of the remote head the
// push left, which is one row of dolt_log: its commit_order is the commit's
// height. Below the bound that is all it does. Once the bound is reached:
//
//  1. it re-roots a scratch branch at that head, leaving it at a height from
//     window.Keep() up to window.ReRootAt() (reRootScratch);
//  2. it force-pushes the scratch to the remote's branch under a lease that
//     names the head (publishReRoot);
//  3. it runs the remote's GC, which replaces the remote's table files with
//     the ones the re-rooted head reaches while the remote carries it
//     (collectRemote);
//  4. it fetches, reads whether the remote carries the re-root, and follows
//     whatever re-root the remote now carries (moveOntoRemoteReRoot). Every
//     lease outcome that reached the remote is settled by that one reading,
//     because each is only the last transport attempt's verdict and an
//     earlier attempt may have landed (storage.LeaseStale,
//     storage.LeaseUnsettled); it is the bound's Publication. A remote a peer
//     pushed to in between carries none, and the branch stays where it was.
//
// The bound collects nothing below the cut. The bound runs on the mirror's
// clone as well as the live store, and a branch that followed leaves the
// store owing the collection, which only the live store's push verb runs,
// after the bound (collectOwedCut).
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
func (s *Store) boundHistory(ctx context.Context, remote storage.RemoteName, branch string, window historywindow.Window) storage.HistoryBound {
	b := storage.HistoryBound{Remote: remote.String(), Branch: branch, Window: window}
	start := time.Now()
	b.Err = s.keepHistoryBound(ctx, remote, &b)
	b.Elapsed = time.Since(start)
	s.bounds = append(s.bounds, b)
	return b
}

// keepHistoryBoundAfter is every push verb's last step: once the push landed,
// it runs boundHistory under HistoryBoundDeadline taken from boundLifetime,
// carries the bound on the push's result, and joins its account to the
// push's maintenance. A push that failed landed nothing to bound. A window
// config left unread keeps no bound, and the maintenance says why. The verbs
// end here rather than leaving it to their caller, so a push through
// storage.Syncer cannot land without the bound. [LAW:no-ambient-temporal-coupling]
func (s *Store) keepHistoryBoundAfter(boundLifetime context.Context, remote storage.RemoteName, branch string, window storage.BoundWindow, pushed storage.SyncPushResult, pushErr error) (storage.SyncPushResult, error) {
	if pushErr != nil {
		return pushed, pushErr
	}
	w, unread := window.Window()
	if unread != nil {
		pushed.Maintenance = joinMaintenance(pushed.Maintenance, "history bound not kept: "+unread.Error())
		return pushed, nil
	}
	ctx, cancel := context.WithTimeout(boundLifetime, HistoryBoundDeadline())
	defer cancel()
	pushed.Bound = s.boundHistory(ctx, remote, strings.TrimSpace(branch), w)
	pushed.Maintenance = joinMaintenance(pushed.Maintenance, historyBoundReport(pushed.Bound))
	return pushed, nil
}

// keepHistoryBound runs boundHistory's steps, filling in b as each one lands.
func (s *Store) keepHistoryBound(ctx context.Context, remote storage.RemoteName, b *storage.HistoryBound) error {
	if _, err := requireSyncArg("branch", b.Branch); err != nil {
		return err
	}
	if err := refuseLOCKUnderCommitLock(ctx); err != nil {
		return err
	}
	trackingRef := fmt.Sprintf("remotes/%s/%s", remote, b.Branch)
	if err := s.db.QueryRowContext(ctx, `SELECT commit_hash, commit_order FROM dolt_log(?) LIMIT 1`, trackingRef).Scan(&b.Head, &b.Height); err != nil {
		return fmt.Errorf("read the height of %s: %w", trackingRef, err)
	}
	if !b.Fired() {
		return nil
	}
	return s.publishBound(ctx, remote, b)
}

// publishBound is boundHistory's steps once the bound fired at b.Head.
func (s *Store) publishBound(ctx context.Context, remote storage.RemoteName, b *storage.HistoryBound) (err error) {
	// The scratch lives as long as the bound needs the re-root's commit, and
	// goes however the bound ended. While the remote lacks the re-root, the
	// scratch is the only ref naming that commit, and the reading resolves
	// it. A re-root is deterministic, so the next push past the bound writes
	// the same commit again, while a scratch left behind would keep the whole
	// history below the cut reachable through every GC until then. One a
	// killed bound left goes with the next collection below a cut
	// (collectBelowTheCut).
	defer func() {
		err = errors.Join(err, s.runSyncMutation(ctx, func(ctx context.Context) error {
			return s.deleteBranchesUnder(ctx, reRootScratchPrefix)
		}))
	}()
	trackingRef := fmt.Sprintf("remotes/%s/%s", remote, b.Branch)
	var publishErr error
	b.ReRooted, publishErr = s.reRootScratch(ctx, b.Branch, b.Head, b.Window)
	if publishErr == nil {
		b.Lease, publishErr = s.publishReRoot(ctx, remote, b.Branch, b.Head)
	}
	// A re-root never written, or a lease push never sent, left the remote
	// without it, so there is nothing to collect, read or follow, and the
	// publication stays ReRootNotPushed.
	// [LAW:types-are-the-program] the lease's empty outcome is that fact,
	// set by the push itself.
	if b.Lease == "" {
		return publishErr
	}
	// [LAW:dataflow-not-control-flow] the remote's GC, the fetch, the reading
	// and the follow run on every lease outcome that was sent: held, stale,
	// and unsettled alike, and no error among them stops the steps after it.
	// The remote's root decides whether its GC replaces anything: only while
	// the remote carries this re-root and nothing on top of it. The reading
	// only classifies the publication, so the follow does not wait on it:
	// this branch follows the re-root the remote carries whatever became of
	// the remote's table files or the reading.
	var remoteGCErr error
	b.RemoteGC, remoteGCErr = s.collectRemote(ctx, remote, b.Branch, b.ReRooted.Head)
	if remoteGCErr != nil {
		remoteGCErr = fmt.Errorf("collect %s: %w", trackingRef, remoteGCErr)
	}
	reading, readErr := s.readPublication(ctx, remote, b, trackingRef)
	b.Publication = publicationOf(b.Lease, reading)
	return errors.Join(publishErr, remoteGCErr, readErr, s.followCarriedReRoot(ctx, remote, b, trackingRef))
}

// remoteReading is what the fetch after a lease push showed of the re-root.
type remoteReading int

const (
	// remoteUnread: no fetch or reading completed.
	remoteUnread remoteReading = iota
	// remoteCarries: the remote's head is the re-root or a descendant of it.
	remoteCarries
	// remoteLacks: the remote's head is neither the re-root nor a descendant of it.
	remoteLacks
)

// publicationOf is how far a re-root whose lease push reached the remote got:
// the reading after it decides, and without one, a held lease is the proof
// it landed, while a stale or unsettled one leaves it possibly published.
// [LAW:single-enforcer] the one place a bound's publication is decided.
func publicationOf(lease storage.LeaseOutcome, reading remoteReading) storage.ReRootPublication {
	switch {
	case reading == remoteCarries:
		return storage.ReRootPublished
	case reading == remoteLacks:
		return storage.ReRootNotCarried
	case lease == storage.LeaseHeld:
		return storage.ReRootPublished
	default:
		return storage.ReRootPossiblyPublished
	}
}

// readPublication is publishBound's reading after the remote's GC: it
// fetches and reads whether the remote carries b's re-root, which the scratch
// branch keeps in this store until the bound ends.
func (s *Store) readPublication(ctx context.Context, remote storage.RemoteName, b *storage.HistoryBound, trackingRef string) (remoteReading, error) {
	if err := s.SyncFetch(ctx, remote); err != nil {
		return remoteUnread, fmt.Errorf("fetch to learn whether %s carries the re-root: %w", trackingRef, err)
	}
	base, err := mergeBase(ctx, s.db, b.ReRooted.Head, trackingRef)
	if err != nil {
		return remoteUnread, fmt.Errorf("read whether %s carries the re-root %s: %w", trackingRef, b.ReRooted.Head, err)
	}
	if commit, shared := base.shared(); shared && commit == b.ReRooted.Head {
		return remoteCarries, nil
	}
	return remoteLacks, nil
}

// followCarriedReRoot follows whatever re-root the remote carries, and
// reports a branch the follow left unrelated to the remote.
func (s *Store) followCarriedReRoot(ctx context.Context, remote storage.RemoteName, b *storage.HistoryBound, trackingRef string) error {
	followed, _, err := s.moveOntoRemoteReRoot(ctx, remote, b.Branch, storage.SyncEntryBound)
	// The follow's record has a head exactly when the branch moved.
	if err != nil || followed.Head != "" {
		return err
	}
	// The follow declines a re-root it cannot recognize as this branch's, and
	// at the other entry points the reconcile then reports the divergence. No
	// reconcile runs here, so the bound reports it: a remote carrying a re-root
	// the branch did not follow, this push's own among them, leaves the branch
	// on history unrelated to it. [LAW:no-silent-failure]
	stranded, err := s.unrelatedToRemote(ctx, remote, b.Branch)
	if err != nil {
		return fmt.Errorf("read whether the branch still shares history with %s: %w", trackingRef, err)
	}
	if stranded {
		return fmt.Errorf("%s carries a re-root the branch did not follow, so the branch is on history unrelated to it", trackingRef)
	}
	return nil
}

// reRootScratch writes the re-root of head onto branch's scratch branch, cut
// where it brings head back inside window (cutIn).
func (s *Store) reRootScratch(ctx context.Context, branch, head string, window historywindow.Window) (storage.ReRooted, error) {
	scratch := ref.NewBranchRef(reRootScratchPrefix + branch)
	var reRooted storage.ReRooted
	err := s.withEngineClosed(ctx, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		headCommit, err := readHistoryCommit(holdCtx, ddb, hash.Parse(head))
		if err != nil {
			return fmt.Errorf("read the remote head: %w", err)
		}
		cut, err := cutIn(holdCtx, ddb, headCommit, window)
		if err != nil {
			return err
		}
		if err := ddb.SetHeadToCommit(holdCtx, scratch, headCommit.commit); err != nil {
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

// cutIn is where a re-root of head brings it back inside window: the highest
// commit every line from head passes through that leaves head at a height
// from window.Keep() up to, not reaching, window.ReRootAt(). Every line
// passing through it is what reRootBranch requires of a cut
// (ErrSideLineAcrossCut), so a cut inside a merge bubble moves down to where
// the bubble opened, keeping more than Keep and never less, which is what lets
// a peer lagging by up to Keep follow. [LAW:one-source-of-truth] the bound
// fires on the height and cuts by it: the longest line passes through the
// cut, so the head the re-root leaves is at height head - cut + 1, Keep
// wherever every line meets at that height, and below ReRootAt, so the next
// push does not fire again. A bound that fired has head at ReRootAt or above,
// so its root, which would keep head's whole height, is never a cut.
//
// The walk takes the highest commit not yet read through, and stops below
// the last height that could be a cut, so it reads at most window.ReRootAt()
// heights of history. Every line from head is waiting at some commit in
// pending, so pending holding only the commit taken means every line passes
// through it. A side line spanning the window leaves no such commit inside
// it: the bound's error, naming where the lines last met above it, which is
// where it cuts once head has climbed Keep above.
func cutIn(ctx context.Context, ddb *doltdb.DoltDB, head historyCommit, window historywindow.Window) (hash.Hash, error) {
	pending := map[hash.Hash]historyCommit{head.addr: head}
	met := head
	for len(pending) > 0 {
		c := slices.MaxFunc(slices.Collect(maps.Values(pending)), func(a, b historyCommit) int {
			return cmp.Compare(a.height, b.height)
		})
		delete(pending, c.addr)
		kept := head.height - c.height + 1
		if kept >= window.ReRootAt() {
			break
		}
		if len(pending) == 0 {
			if kept >= window.Keep() {
				return c.addr, nil
			}
			met = c
		}
		for _, p := range c.parents {
			if _, ok := pending[p]; ok {
				continue
			}
			parent, err := readHistoryCommit(ctx, ddb, p)
			if err != nil {
				return hash.Hash{}, err
			}
			pending[p] = parent
		}
	}
	return hash.Hash{}, fmt.Errorf("no commit every line below %s passes through leaves it at a height from %d to %d; the lowest they meet at above that is %s, at height %d, where the bound cuts once the head is at height %d", head.addr, window.Keep(), window.ReRootAt()-1, met.addr, met.height, met.height+window.Keep()-1)
}

// publishReRoot force-pushes branch's scratch to the remote's branch, landing
// only while that branch still names head. The outcome is none only when no
// attempt reached the remote: the mutation retries its whole body on GC
// contention, and an attempt that stops short of the remote does not undo an
// earlier one that reached it.
func (s *Store) publishReRoot(ctx context.Context, remote storage.RemoteName, branch string, head string) (storage.LeaseOutcome, error) {
	var outcome storage.LeaseOutcome
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
		attempt, err := s.pushLeasedWithinLock(ctx, remote, reRootScratchPrefix+branch, branch, head)
		outcome = cmp.Or(attempt, outcome)
		return err
	})
	return outcome, err
}

// collectBelowTheCut reclaims the history below the cut once the sync branch
// has followed a re-root, its fork points with it (moveOntoRemoteReRoot):
// under one commit lock, it drops the migration checkpoints, the reconcile
// scratch branches and any re-root scratch a killed bound left, every one of
// which names a commit from before the re-root, and runs the full GC. Every
// follow runs after a fetch moved the tracking ref onto the re-rooted head;
// while that ref still named the old head, the same GC freed nothing
// (links-storage-3k6u.ge9). A ref it could not drop still pins that history,
// so it stops the pass before the GC.
//
// The store is measured around the pass, outside the lock, as SyncCompact
// measures it: the two readings are an account of what happened and never an
// input to it, so a reading that fails joins the collection's error and stops
// nothing. The collection's error is its own (storage.Collection.Err).
func (s *Store) collectBelowTheCut(ctx context.Context) storage.Collection {
	var c storage.Collection
	var beforeErr, afterErr error
	c.BytesBefore, beforeErr = ChunkStoreBytes(s.doltRootDir)
	var sweep refSweepOutcome
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
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
	c.BytesAfter, afterErr = ChunkStoreBytes(s.doltRootDir)
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
		parts = append(parts, fmt.Sprintf("store %s -> %s", storage.FormatBytes(c.BytesBefore), storage.FormatBytes(c.BytesAfter)))
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
// and once the bound fired, the re-root it wrote and how far that got toward
// the remote, or where it stopped.
func historyBoundReport(b storage.HistoryBound) string {
	var parts []string
	if b.Fired() {
		parts = append(parts, fmt.Sprintf("history bound: the remote head reached height %d (bound %d)", b.Height, b.Window.ReRootAt()))
	}
	if b.ReRooted.Head != "" {
		parts = append(parts, fmt.Sprintf("re-root at %s, keeping %d commits and dropping %d: %s", b.ReRooted.Cut, b.ReRooted.Kept(), b.ReRooted.Dropped, b.Publication))
	}
	if b.Lease != "" {
		parts = append(parts, "lease "+string(b.Lease))
	}
	if g := b.RemoteGC; g.Outcome != "" {
		parts = append(parts, fmt.Sprintf("remote GC %s: %s of table files, %s collected, %d other branches dropped", g.Outcome, storage.FormatBytes(g.BytesBefore), storage.FormatBytes(g.CollectedBytes), len(g.DroppedBranches)))
	}
	if b.Err != nil {
		parts = append(parts, fmt.Sprintf("history bound not kept: %v", b.Err))
	}
	return joinMaintenance(parts...)
}
