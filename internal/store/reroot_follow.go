package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// followRemoteReRoot brings the local branch into the remote's history when
// another checkout re-rooted that history and force-pushed it (ReRoot). It
// reads the remote-tracking ref as the last fetch left it and contacts no
// network.
//
// The remote is followed when the two histories share no commit and the
// remote's one root is exactly the parentless re-commit of a commit C the
// local branch holds. The local branch is then re-rooted at C, and since a
// re-root is deterministic it arrives at the remote's own hashes for C and for
// every commit after C that both sides hold. What is left is an ordinary
// fast-forward or field-aware merge, which the caller's settle performs.
//
// C is recognized by recomputation and never by resemblance: the local commit
// whose metadata equals the root's is rebuilt parentless, and it is C only if
// that rebuild hashes to the remote root. Metadata alone matches two stores
// that never shared a write, and following one of those would merge them
// silently. [LAW:parse-dont-validate] the recomputed hash is the one evidence
// of a re-root.
//
// Histories that share a commit, a remote never fetched, and an unrelated
// remote that is not a re-root of this branch are all left exactly as they
// are; the last stays the unrelated-histories divergence the reconcile
// reports.
//
// The follow itself is moveOntoRemoteReRoot. Every caller of this one runs on
// a store that outlives the sync, so once the branch followed it collects
// below the cut (collectOwedCut): a checkout that follows drops the history
// the re-root dropped from its store too, and not only the checkout whose
// push re-rooted it. The follow and the collection are recorded under entry,
// for FollowedReRoots and Collections. A cut recognized and not followed,
// because a side line goes around it, is recorded under entry with its
// reason, for DeclinedReRoots.
//
// Every public sync entry point that reads the tracking ref runs it first, so
// none of them meets a re-root it could follow as a divergence; the private
// settle and reconcile cores never do, because SyncPull composes them under
// one commit lock. The follow takes Dolt's LOCK to rewrite the branch, closing
// and reopening this store's own engine around it, so it refuses to run under
// the commit lock, whether or not this run would follow: LOCK comes before
// the commit lock (doc.go).
// [LAW:no-ambient-temporal-coupling]
func (s *Store) followRemoteReRoot(ctx context.Context, remote storage.RemoteName, branch string, entry storage.SyncEntry) error {
	if _, _, err := s.moveOntoRemoteReRoot(ctx, remote, branch, entry); err != nil {
		return err
	}
	s.collectOwedCut(ctx, entry)
	return nil
}

// moveOntoRemoteReRoot is the follow itself, on any store: it re-roots the
// branch onto the remote's history, records the follow for FollowedReRoots,
// or a declined cut or what its hold read instead of a cut, and once the move
// finished, moves remote's fork points across the cut. The returned record
// has a head exactly when the branch moved, with the move's error beside it.
// crossed is whether a branch the probe read as unrelated to the remote
// shares its history when the hold ends, by this move or by another writer's
// follow before the hold.
//
// A follow whose fork points crossed leaves the store owing the collection
// below the cut (collectOwedCut), which only a verb on a store that outlives
// the sync runs: on the mirror's clone, which is deleted when its cycle ends,
// the debt goes with it. [LAW:decomposition] the move and its fork points are the follow;
// what a store does across the cut afterwards is maintenance.
func (s *Store) moveOntoRemoteReRoot(ctx context.Context, remote storage.RemoteName, branch string, entry storage.SyncEntry) (followed storage.FollowedReRoot, crossed bool, err error) {
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return storage.FollowedReRoot{}, false, err
	}
	if err := refuseLOCKUnderCommitLock(ctx); err != nil {
		return storage.FollowedReRoot{}, false, wrapFollowErr(remote, trimmedBranch, err)
	}
	unrelated, err := s.unrelatedToRemote(ctx, remote, trimmedBranch)
	if err != nil {
		return storage.FollowedReRoot{}, false, err
	}
	// The probe decides only whether a hold is worth closing the engine for:
	// only histories that share no commit can be a re-root to follow. It
	// decides nothing about the follow, since another writer can follow in
	// the gap before the hold; the hold reads the branch, the tracking ref and
	// their relation again. [LAW:no-ambient-temporal-coupling]
	if !unrelated {
		return storage.FollowedReRoot{}, false, nil
	}
	followed, crossed, err = s.followUnderHold(ctx, remote, trimmedBranch, entry)
	if followed.Head == "" || err != nil {
		return followed, crossed, err
	}
	// The fetch that brought the re-root kept the old fork points, since the
	// branch then shared no commit with the tracking refs; left there, a
	// fork point below the cut pins that history through every GC and is
	// the base a later reconcile merges against. [LAW:no-silent-failure] a
	// fork-point move that fails is the follow's error, beside a branch that
	// moved. A collection frees nothing below this cut while a fork point
	// pins it, so only fork points that crossed leave the store owing one; a
	// debt an earlier follow left stands, since the history below its cut no
	// longer has them to pin it.
	moveErr := s.runSyncMutation(ctx, func(ctx context.Context) error {
		if err := s.moveForkPointsAcrossTheCut(ctx, remote); err != nil {
			return fmt.Errorf("move fork points across the cut: %w", err)
		}
		return nil
	})
	s.owesCollection = s.owesCollection || moveErr == nil
	return followed, crossed, wrapFollowErr(remote, trimmedBranch, moveErr)
}

// followUnderHold is moveOntoRemoteReRoot after its probe: under the hold
// that rewrites the branch, it re-roots the local branch onto
// remotes/<remote>/<branch> if that tracking ref's history is a re-root the
// branch can follow when the hold opens, and records the follow, a declined
// cut, or what the hold read instead of a cut. The returned record has a head
// exactly when the branch moved, and crossed is whether the branch shares the
// remote's history when the hold ends: it moved, or the hold read the
// histories sharing a commit.
func (s *Store) followUnderHold(ctx context.Context, remote storage.RemoteName, trimmedBranch string, entry storage.SyncEntry) (followed storage.FollowedReRoot, crossed bool, _ error) {
	followed = storage.FollowedReRoot{Entry: entry}
	var declined []storage.DeclinedReRoot
	var notFound []storage.CutNotFound
	err := s.withEngineClosed(ctx, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		local, err := localBranchAt(s.doltRootDir)
		if err != nil {
			return err
		}
		followed.ReRooted, declined, notFound, err = followReRoot(holdCtx, ddb, local, ref.NewRemoteRef(remote.String(), trimmedBranch), entry)
		return err
	})
	// A cut recognized and not followed, and a hold that found no cut, are
	// the store's decisions too, recorded with their reasons; the branch has
	// not moved. [LAW:nothing-unseen]
	s.declined = append(s.declined, declined...)
	s.cutsNotFound = append(s.cutsNotFound, notFound...)
	// reRootBranch sets Head exactly when the branch moved, whatever the error.
	if followed.Head == "" {
		crossed = slices.ContainsFunc(notFound, func(c storage.CutNotFound) bool { return c.Reason == storage.CutNotFoundSharedHistory })
		return storage.FollowedReRoot{}, crossed, wrapFollowErr(remote, trimmedBranch, err)
	}
	// A branch that moved stands even when something after the move failed,
	// as ReRoot's own result does, so the follow is recorded either way.
	// [LAW:no-silent-failure]
	s.followed = append(s.followed, followed)
	return followed, true, wrapFollowErr(remote, trimmedBranch, err)
}

// collectOwedCut is the collection below the cut that a verb on a store
// which outlives the sync runs once a follow left the branch across one
// (moveOntoRemoteReRoot): collectBelowTheCut, recorded under entry for
// Collections. A store that owes none collects and records nothing. The
// attempt clears the debt whatever it did, so a failed collection is
// reported once and never re-run by the next step of the same sync.
// [LAW:dataflow-not-control-flow] every live verb calls it; the debt decides.
func (s *Store) collectOwedCut(ctx context.Context, entry storage.SyncEntry) storage.Collection {
	if !s.owesCollection {
		return storage.Collection{}
	}
	s.owesCollection = false
	c := s.collectBelowTheCut(ctx)
	c.Entry = entry
	s.collections = append(s.collections, c)
	return c
}

// moveForkPointsAcrossTheCut moves remote's fork points onto the history
// their tracking refs now name, inside the caller's commit lock, once a
// follow moved the branch onto it.
func (s *Store) moveForkPointsAcrossTheCut(ctx context.Context, remote storage.RemoteName) error {
	if s.moveForkPointsHookForTest != nil {
		if err := s.moveForkPointsHookForTest(); err != nil {
			return err
		}
	}
	tracked, err := s.trackingRefs(ctx, ownerOf(remote))
	if err != nil {
		return err
	}
	forkPoints, err := s.recordForkPoints(ctx, ownerOf(remote), tracked)
	s.forkPoints = append(s.forkPoints, forkPoints...)
	return err
}

// wrapFollowErr is a follow's error as it reaches the entry point, naming the
// remote branch it followed; nil stays nil.
func wrapFollowErr(remote storage.RemoteName, branch string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("follow a re-root of %s/%s: %w", remote, branch, err)
}

// SyncFollowReRoot is followRemoteReRoot for a caller that fetched and
// settles nothing. See the storage.Syncer contract.
func (s *Store) SyncFollowReRoot(ctx context.Context, remote storage.RemoteName, branch string, entry storage.SyncEntry) error {
	return s.followRemoteReRoot(ctx, remote, branch, entry)
}

// FollowedReRoots is every re-root moveOntoRemoteReRoot applied on this store
// since it opened, oldest first.
func (s *Store) FollowedReRoots() []storage.FollowedReRoot {
	return slices.Clone(s.followed)
}

// Collections is every collection below a cut collectOwedCut ran on this
// store since it opened, oldest first.
func (s *Store) Collections() []storage.Collection {
	return slices.Clone(s.collections)
}

// DeclinedReRoots is every re-root cut moveOntoRemoteReRoot recognized on this
// store since it opened and did not follow, oldest first.
func (s *Store) DeclinedReRoots() []storage.DeclinedReRoot {
	return slices.Clone(s.declined)
}

// CutsNotFound is every follow on this store since it opened whose hold
// found no cut, oldest first.
func (s *Store) CutsNotFound() []storage.CutNotFound {
	return slices.Clone(s.cutsNotFound)
}

// unrelatedToRemote reads whether the active branch shares no commit with
// remotes/<remote>/<branch>. A ref never fetched relates to nothing and is
// not unrelated either: there is nothing on the other side.
func (s *Store) unrelatedToRemote(ctx context.Context, remote storage.RemoteName, branch string) (unrelated bool, err error) {
	trackingRef := fmt.Sprintf("remotes/%s/%s", remote, branch)
	tracked, err := trackingRefExists(ctx, s.db, trackingRef)
	if err != nil || !tracked {
		return false, err
	}
	local, err := activeBranch(ctx, s.db)
	if err != nil {
		return false, err
	}
	base, err := mergeBase(ctx, s.db, local, trackingRef)
	if err != nil {
		return false, err
	}
	_, shared := base.shared()
	return !shared, nil
}

// withEngineClosed closes this store's engine, runs write under
// holdForRefWrite — LOCK, then the commit lock — within ReRootHoldBudget, and
// reopens the engine whatever happened, so the store is usable again on every
// return. A follow and the bound's re-root both rewrite history this way.
// [LAW:single-enforcer] The reopen restores the store rather than doing the
// caller's work, so a deadline that cut the work does not cut it; the
// engine open's own wait on Dolt's LOCK bounds it.
func (s *Store) withEngineClosed(ctx context.Context, write func(holdCtx context.Context, ddb *doltdb.DoltDB) error) error {
	if err := refuseLOCKUnderCommitLock(ctx); err != nil {
		return err
	}
	reopenCtx := context.WithoutCancel(ctx)
	if err := s.closeEngine(); err != nil {
		return errors.Join(fmt.Errorf("close the engine: %w", err), s.reopenEngine(reopenCtx))
	}
	holdErr := holdForRefWrite(ctx, s.stderr, s.doltRootDir, engineWrite, ReRootHoldBudget, write)
	return errors.Join(holdErr, s.reopenEngine(reopenCtx))
}

// reopenEngine opens the engine closeEngine closed, under the store's own
// open contract. A failure leaves the closed engine in place, so every query
// after it fails loudly.
func (s *Store) reopenEngine(ctx context.Context) error {
	db, hold, releaseEngineRecord, err := openEngine(ctx, s.stderr, s.doltRootDir, s.workspaceID, s.access, s.phase)
	if err != nil {
		return fmt.Errorf("reopen the engine: %w", err)
	}
	s.db, s.hold, s.releaseEngineRecord = db, hold, releaseEngineRecord
	return s.resumeCheckedOut(ctx)
}

// followReRoot is the follow inside the hold: recognize the cut, then re-root
// branch at it. followed.Head is set exactly when the branch moved, which a
// re-root that failed after its move still did; it is empty, with nothing
// written to any ref, when the tracking ref's history is not a re-root branch
// can follow. Under entry, declined holds the one cut it recognized and could
// not follow, and notFound what it read when it recognized no cut; each is
// empty otherwise.
func followReRoot(ctx context.Context, ddb *doltdb.DoltDB, branch, tracking ref.DoltRef, entry storage.SyncEntry) (followed storage.ReRooted, declined []storage.DeclinedReRoot, notFound []storage.CutNotFound, err error) {
	cut, missing, err := recognizeReRootCut(ctx, ddb, branch, tracking)
	if err != nil {
		return storage.ReRooted{}, nil, nil, err
	}
	if missing != "" {
		return storage.ReRooted{}, nil, []storage.CutNotFound{{Entry: entry, Reason: missing}}, nil
	}
	followed, err = reRootBranch(ctx, ddb, branch, cut)
	if errors.Is(err, ErrSideLineAcrossCut) {
		// A line of the local history goes around the cut, so no re-root of
		// it reproduces the remote's: not a history this branch can follow.
		return storage.ReRooted{}, []storage.DeclinedReRoot{{Entry: entry, Cut: cut.String(), Reason: err.Error()}}, nil, nil
	}
	return followed, nil, nil, err
}

// recognizeReRootCut finds the commit of branch's history whose parentless
// re-commit is the one root of tracking's history: the cut, since the re-root
// at it writes that same re-commit and so arrives at the remote's root.
// missing is empty exactly when cut is that commit, and otherwise is what it
// read instead: tracking gone, the two histories sharing a commit,
// tracking's history with more than one root, or not exactly one local
// commit rebuilding to its root. Histories that share a commit are not a
// re-root to follow, whatever the remote's shape: this is where the follow
// reads that the histories are unrelated, under the hold that rewrites the
// branch. [LAW:no-ambient-temporal-coupling]
//
// Rebuilding a candidate writes its parentless commit, as the re-root that
// follows would; a candidate that does not match leaves that commit dangling
// for GC, and no ref names it.
func recognizeReRootCut(ctx context.Context, ddb *doltdb.DoltDB, branch, tracking ref.DoltRef) (cut hash.Hash, missing storage.CutNotFoundReason, err error) {
	localHead, err := resolveRefHash(ctx, ddb, branch)
	if err != nil {
		return hash.Hash{}, "", err
	}
	remoteHead, tracked, err := resolveRefHead(ctx, ddb, tracking)
	if err != nil {
		return hash.Hash{}, "", fmt.Errorf("resolve %s: %w", tracking, err)
	}
	if !tracked {
		return hash.Hash{}, storage.CutNotFoundNoTrackingRef, nil
	}
	remoteHistory, err := readHistory(ctx, ddb, remoteHead, nil)
	if err != nil {
		return hash.Hash{}, "", fmt.Errorf("read the remote's history: %w", err)
	}
	localHistory, err := readHistory(ctx, ddb, localHead, nil)
	if err != nil {
		return hash.Hash{}, "", fmt.Errorf("read the local history: %w", err)
	}
	for addr := range remoteHistory {
		if _, shared := localHistory[addr]; shared {
			return hash.Hash{}, storage.CutNotFoundSharedHistory, nil
		}
	}
	var roots []historyCommit
	for _, c := range remoteHistory {
		if len(c.parents) == 0 {
			roots = append(roots, c)
		}
	}
	if len(roots) != 1 {
		return hash.Hash{}, storage.CutNotFoundNotOneRoot, nil
	}
	root := roots[0]
	rootMeta, err := root.commit.GetCommitMeta(ctx)
	if err != nil {
		return hash.Hash{}, "", err
	}
	var matches []hash.Hash
	for addr, c := range localHistory {
		meta, err := c.commit.GetCommitMeta(ctx)
		if err != nil {
			return hash.Hash{}, "", err
		}
		if *meta != *rootMeta {
			continue
		}
		rebuilt, err := commitRoot(ctx, ddb, c.commit)
		if err != nil {
			return hash.Hash{}, "", err
		}
		rebuiltAddr, err := rebuilt.HashOf()
		if err != nil {
			return hash.Hash{}, "", err
		}
		if rebuiltAddr == root.addr {
			matches = append(matches, addr)
		}
	}
	// Two local commits that both rebuild to the root differ only in their
	// parents, and each would re-root the branch differently: no one cut is
	// evidenced.
	if len(matches) != 1 {
		return hash.Hash{}, storage.CutNotFoundNotOneRebuild, nil
	}
	return matches[0], "", nil
}

// resolveRefHash is the commit r names.
func resolveRefHash(ctx context.Context, ddb *doltdb.DoltDB, r ref.DoltRef) (hash.Hash, error) {
	c, err := ddb.ResolveCommitRef(ctx, r)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("resolve %s: %w", r, err)
	}
	return c.HashOf()
}
