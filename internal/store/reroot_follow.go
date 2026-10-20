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
// A branch that followed is across the cut, so the store then reclaims what
// is below it (collectBelowTheCut): whichever checkout follows, the history
// the re-root dropped leaves its store too, and not only the store of the
// checkout whose push re-rooted it. The collection is maintenance: its
// failure is carried on the record and never fails the sync that followed,
// as a push's other maintenance never fails the push. The follow and its
// collection are recorded under entry, for FollowedReRoots, and returned; the
// record is zero when the branch did not move. A cut recognized and not
// followed, because a side line goes around it, is recorded under entry with
// its reason, for DeclinedReRoots.
//
// Every public sync entry point that reads the tracking ref runs it first, so
// none of them meets a re-root it could follow as a divergence; the private
// settle and reconcile cores never do, because SyncPull composes them under
// one commit lock. The follow takes Dolt's LOCK to rewrite the branch, closing
// and reopening this store's own engine around it, so it refuses to run under
// the commit lock, whether or not this run would follow: LOCK comes before
// the commit lock (doc.go).
// [LAW:no-ambient-temporal-coupling]
func (s *Store) followRemoteReRoot(ctx context.Context, remote storage.RemoteName, branch string, entry storage.SyncEntry) (storage.FollowedReRoot, error) {
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return storage.FollowedReRoot{}, err
	}
	if err := refuseLOCKUnderCommitLock(ctx); err != nil {
		return storage.FollowedReRoot{}, wrapFollowErr(remote, trimmedBranch, err)
	}
	local, unrelated, err := s.unrelatedToRemote(ctx, remote, trimmedBranch)
	if err != nil {
		return storage.FollowedReRoot{}, err
	}
	// The one branch is the relation the probe read: only histories that share
	// no commit can be a re-root to follow, and finding out costs the engine.
	// [LAW:dataflow-not-control-flow]
	if !unrelated {
		return storage.FollowedReRoot{}, nil
	}
	followed := storage.FollowedReRoot{Entry: entry}
	var declined []storage.DeclinedReRoot
	err = s.withEngineClosed(ctx, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		var err error
		followed.ReRooted, declined, err = followReRoot(holdCtx, ddb, ref.NewBranchRef(local), ref.NewRemoteRef(remote.String(), trimmedBranch), entry)
		return err
	})
	// A cut recognized and not followed is the store's decision too, recorded
	// with its reason; the branch has not moved. [LAW:nothing-unseen]
	s.declined = append(s.declined, declined...)
	// reRootBranch sets Head exactly when the branch moved, whatever the error.
	if followed.Head == "" {
		return storage.FollowedReRoot{}, wrapFollowErr(remote, trimmedBranch, err)
	}
	// A branch that moved stands even when something after the move failed,
	// as ReRoot's own result does, so the follow is recorded either way; only
	// a follow that finished is across the cut and collects below it.
	// [LAW:no-silent-failure]
	if err == nil {
		followed.Collection = s.collectBelowTheCut(ctx, remote)
	}
	s.followed = append(s.followed, followed)
	return followed, wrapFollowErr(remote, trimmedBranch, err)
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
	_, err := s.followRemoteReRoot(ctx, remote, branch, entry)
	return err
}

// FollowedReRoots is every re-root followRemoteReRoot applied on this store
// since it opened, oldest first.
func (s *Store) FollowedReRoots() []storage.FollowedReRoot {
	return slices.Clone(s.followed)
}

// DeclinedReRoots is every re-root cut followRemoteReRoot recognized on this
// store since it opened and did not follow, oldest first.
func (s *Store) DeclinedReRoots() []storage.DeclinedReRoot {
	return slices.Clone(s.declined)
}

// unrelatedToRemote reads the active branch and whether it shares no commit
// with remotes/<remote>/<branch>. A ref never fetched relates to nothing and
// is not unrelated either: there is nothing on the other side.
func (s *Store) unrelatedToRemote(ctx context.Context, remote storage.RemoteName, branch string) (local string, unrelated bool, err error) {
	trackingRef := fmt.Sprintf("remotes/%s/%s", remote, branch)
	tracked, err := trackingRefExists(ctx, s.db, trackingRef)
	if err != nil || !tracked {
		return "", false, err
	}
	local, err = activeBranch(ctx, s.db)
	if err != nil {
		return "", false, err
	}
	base, err := mergeBase(ctx, s.db, local, trackingRef)
	if err != nil {
		return "", false, err
	}
	_, shared := base.shared()
	return local, !shared, nil
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
	holdErr := holdForRefWrite(ctx, s.doltRootDir, ReRootHoldBudget, write)
	return errors.Join(holdErr, s.reopenEngine(reopenCtx))
}

// reopenEngine opens the engine closeEngine closed, under the store's own
// open contract. A failure leaves the closed engine in place, so every query
// after it fails loudly.
func (s *Store) reopenEngine(ctx context.Context) error {
	db, releaseEngineRecord, err := openEngine(ctx, s.doltRootDir, s.workspaceID, s.access)
	if err != nil {
		return fmt.Errorf("reopen the engine: %w", err)
	}
	s.db, s.releaseEngineRecord = db, releaseEngineRecord
	return nil
}

// followReRoot is the follow inside the hold: recognize the cut, then re-root
// branch at it. followed.Head is set exactly when the branch moved, which a
// re-root that failed after its move still did; it is empty, with nothing
// written to any ref, when the tracking ref's history is not a re-root branch
// can follow. declined holds the one cut it recognized and could not follow,
// under entry, and is empty otherwise.
func followReRoot(ctx context.Context, ddb *doltdb.DoltDB, branch, tracking ref.DoltRef, entry storage.SyncEntry) (followed storage.ReRooted, declined []storage.DeclinedReRoot, err error) {
	cut, ok, err := recognizeReRootCut(ctx, ddb, branch, tracking)
	if err != nil || !ok {
		return storage.ReRooted{}, nil, err
	}
	followed, err = reRootBranch(ctx, ddb, branch, cut)
	if errors.Is(err, ErrSideLineAcrossCut) {
		// A line of the local history goes around the cut, so no re-root of
		// it reproduces the remote's: not a history this branch can follow.
		return storage.ReRooted{}, []storage.DeclinedReRoot{{Entry: entry, Cut: cut.String(), Reason: err.Error()}}, nil
	}
	return followed, nil, err
}

// recognizeReRootCut finds the commit of branch's history whose parentless
// re-commit is the one root of tracking's history: the cut, since the re-root
// at it writes that same re-commit and so arrives at the remote's root. ok is
// false when tracking's
// history has more than one root, or when not exactly one local commit rebuilds
// to it. The caller has read that the two histories share no commit, so the
// local history does not hold the root itself.
//
// Rebuilding a candidate writes its parentless commit, as the re-root that
// follows would; a candidate that does not match leaves that commit dangling
// for GC, and no ref names it.
func recognizeReRootCut(ctx context.Context, ddb *doltdb.DoltDB, branch, tracking ref.DoltRef) (cut hash.Hash, ok bool, err error) {
	localHead, err := resolveRefHash(ctx, ddb, branch)
	if err != nil {
		return hash.Hash{}, false, err
	}
	remoteHead, err := resolveRefHash(ctx, ddb, tracking)
	if err != nil {
		return hash.Hash{}, false, err
	}
	remoteHistory, err := readHistory(ctx, ddb, remoteHead, nil)
	if err != nil {
		return hash.Hash{}, false, fmt.Errorf("read the remote's history: %w", err)
	}
	var roots []historyCommit
	for _, c := range remoteHistory {
		if len(c.parents) == 0 {
			roots = append(roots, c)
		}
	}
	if len(roots) != 1 {
		return hash.Hash{}, false, nil
	}
	root := roots[0]
	rootMeta, err := root.commit.GetCommitMeta(ctx)
	if err != nil {
		return hash.Hash{}, false, err
	}
	localHistory, err := readHistory(ctx, ddb, localHead, nil)
	if err != nil {
		return hash.Hash{}, false, fmt.Errorf("read the local history: %w", err)
	}
	var matches []hash.Hash
	for addr, c := range localHistory {
		meta, err := c.commit.GetCommitMeta(ctx)
		if err != nil {
			return hash.Hash{}, false, err
		}
		if *meta != *rootMeta {
			continue
		}
		rebuilt, err := commitRoot(ctx, ddb, c.commit)
		if err != nil {
			return hash.Hash{}, false, err
		}
		rebuiltAddr, err := rebuilt.HashOf()
		if err != nil {
			return hash.Hash{}, false, err
		}
		if rebuiltAddr == root.addr {
			matches = append(matches, addr)
		}
	}
	// Two local commits that both rebuild to the root differ only in their
	// parents, and each would re-root the branch differently: no one cut is
	// evidenced.
	if len(matches) != 1 {
		return hash.Hash{}, false, nil
	}
	return matches[0], true, nil
}

// resolveRefHash is the commit r names.
func resolveRefHash(ctx context.Context, ddb *doltdb.DoltDB, r ref.DoltRef) (hash.Hash, error) {
	c, err := ddb.ResolveCommitRef(ctx, r)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("resolve %s: %w", r, err)
	}
	return c.HashOf()
}
