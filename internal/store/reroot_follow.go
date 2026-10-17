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
// reports. A follow is recorded under entry, for FollowedReRoots.
//
// Every public sync entry point that reads the tracking ref runs it first, so
// none of them meets a re-root it could follow as a divergence; the private
// settle and reconcile cores never do, because SyncPull composes them under
// one commit lock. The follow takes Dolt's LOCK to rewrite the branch, closing
// and reopening this store's own engine around it, so it refuses to run under
// the commit lock: LOCK comes before the commit lock (doc.go).
// [LAW:no-ambient-temporal-coupling]
func (s *Store) followRemoteReRoot(ctx context.Context, remote, branch string, entry storage.SyncEntry) (followed bool, err error) {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return false, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return false, err
	}
	if since, held := commitLockHeldSince(ctx); held {
		return false, fmt.Errorf("follow a re-root of %s/%s: the commit lock is held (since %s), and the follow takes Dolt's LOCK, which is taken before the commit lock or not at all", trimmedRemote, trimmedBranch, since.Format("15:04:05.000"))
	}
	local, unrelated, err := s.unrelatedToRemote(ctx, trimmedRemote, trimmedBranch)
	if err != nil {
		return false, err
	}
	// The one branch is the relation the probe read: only histories that share
	// no commit can be a re-root to follow, and finding out costs the engine.
	// [LAW:dataflow-not-control-flow]
	if !unrelated {
		return false, nil
	}
	reRooted, ok, err := s.followWithEngineClosed(ctx, local, ref.NewRemoteRef(trimmedRemote, trimmedBranch))
	if err != nil {
		return false, fmt.Errorf("follow a re-root of %s/%s: %w", trimmedRemote, trimmedBranch, err)
	}
	if ok {
		s.followed = append(s.followed, storage.FollowedReRoot{Entry: entry, ReRooted: reRooted})
	}
	return ok, nil
}

// FollowedReRoots is every re-root followRemoteReRoot applied on this store
// since it opened, oldest first.
func (s *Store) FollowedReRoots() []storage.FollowedReRoot {
	return slices.Clone(s.followed)
}

// unrelatedToRemote reads the active branch and whether it shares no commit
// with remotes/<remote>/<branch>. A ref never fetched relates to nothing and
// is not unrelated either: there is nothing on the other side.
func (s *Store) unrelatedToRemote(ctx context.Context, remote, branch string) (local string, unrelated bool, err error) {
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

// followWithEngineClosed closes this store's engine, follows under
// holdForRefWrite — LOCK, then the commit lock — and reopens the engine
// whatever happened, so the store is usable again on every return.
func (s *Store) followWithEngineClosed(ctx context.Context, local string, tracking ref.DoltRef) (followed storage.ReRooted, ok bool, err error) {
	if err := s.closeEngine(); err != nil {
		return storage.ReRooted{}, false, errors.Join(fmt.Errorf("close the engine: %w", err), s.reopenEngine(ctx))
	}
	holdErr := holdForRefWrite(ctx, s.doltRootDir, ReRootHoldBudget, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		var err error
		followed, ok, err = followReRoot(holdCtx, ddb, ref.NewBranchRef(local), tracking)
		return err
	})
	return followed, ok, errors.Join(holdErr, s.reopenEngine(ctx))
}

// reopenEngine opens the engine closeEngine closed, under the store's own
// open contract. A failure leaves the closed engine in place, so every query
// after it fails loudly, and a holder record that is no longer held is not
// retired twice.
func (s *Store) reopenEngine(ctx context.Context) error {
	db, releaseEngineRecord, err := openEngine(ctx, s.doltRootDir, s.workspaceID, s.access)
	if err != nil {
		s.releaseEngineRecord = func() error { return nil }
		return fmt.Errorf("reopen the engine: %w", err)
	}
	s.db, s.releaseEngineRecord = db, releaseEngineRecord
	return nil
}

// followReRoot is the follow inside the hold: recognize the cut, then re-root
// branch at it. ok is false, with nothing written to any ref, when the
// tracking ref's history is not a re-root branch can follow.
func followReRoot(ctx context.Context, ddb *doltdb.DoltDB, branch, tracking ref.DoltRef) (storage.ReRooted, bool, error) {
	cut, ok, err := recognizeReRootCut(ctx, ddb, branch, tracking)
	if err != nil || !ok {
		return storage.ReRooted{}, false, err
	}
	followed, err := reRootBranch(ctx, ddb, branch, cut.local)
	if errors.Is(err, ErrSideLineAcrossCut) {
		// A line of the local history goes around the cut, so no re-root of
		// it reproduces the remote's: not a history this branch can follow.
		return storage.ReRooted{}, false, nil
	}
	if err != nil {
		return storage.ReRooted{}, false, err
	}
	if followed.Root != cut.remoteRoot.String() {
		return followed, true, fmt.Errorf("the re-root at %s wrote the root %s, not the remote's %s it was recognized by", cut.local, followed.Root, cut.remoteRoot)
	}
	return followed, true, nil
}

// reRootCut is a recognized re-root: the local commit whose parentless
// re-commit is the remote's root.
type reRootCut struct {
	local      hash.Hash
	remoteRoot hash.Hash
}

// recognizeReRootCut finds the commit of branch's history whose parentless
// re-commit is the one root of tracking's history. ok is false when tracking's
// history has more than one root, when branch already holds the root (the two
// share history), or when not exactly one local commit rebuilds to it.
//
// Rebuilding a candidate writes its parentless commit, as the re-root that
// follows would; a candidate that does not match leaves that commit dangling
// for GC, and no ref names it.
func recognizeReRootCut(ctx context.Context, ddb *doltdb.DoltDB, branch, tracking ref.DoltRef) (reRootCut, bool, error) {
	localHead, err := resolveRefHash(ctx, ddb, branch)
	if err != nil {
		return reRootCut{}, false, err
	}
	remoteHead, err := resolveRefHash(ctx, ddb, tracking)
	if err != nil {
		return reRootCut{}, false, err
	}
	remoteHistory, err := readHistory(ctx, ddb, remoteHead, nil)
	if err != nil {
		return reRootCut{}, false, fmt.Errorf("read the remote's history: %w", err)
	}
	var roots []historyCommit
	for _, c := range remoteHistory {
		if len(c.parents) == 0 {
			roots = append(roots, c)
		}
	}
	if len(roots) != 1 {
		return reRootCut{}, false, nil
	}
	root := roots[0]
	rootMeta, err := root.commit.GetCommitMeta(ctx)
	if err != nil {
		return reRootCut{}, false, err
	}
	localHistory, err := readHistory(ctx, ddb, localHead, nil)
	if err != nil {
		return reRootCut{}, false, fmt.Errorf("read the local history: %w", err)
	}
	if _, holds := localHistory[root.addr]; holds {
		return reRootCut{}, false, nil
	}
	var matches []hash.Hash
	for addr, c := range localHistory {
		meta, err := c.commit.GetCommitMeta(ctx)
		if err != nil {
			return reRootCut{}, false, err
		}
		if *meta != *rootMeta {
			continue
		}
		rebuilt, err := commitRoot(ctx, ddb, c.commit)
		if err != nil {
			return reRootCut{}, false, err
		}
		rebuiltAddr, err := rebuilt.HashOf()
		if err != nil {
			return reRootCut{}, false, err
		}
		if rebuiltAddr == root.addr {
			matches = append(matches, addr)
		}
	}
	// Two local commits that both rebuild to the root differ only in their
	// parents, and each would re-root the branch differently: no one cut is
	// evidenced.
	if len(matches) != 1 {
		return reRootCut{}, false, nil
	}
	return reRootCut{local: matches[0], remoteRoot: root.addr}, true, nil
}

// resolveRefHash is the commit r names.
func resolveRefHash(ctx context.Context, ddb *doltdb.DoltDB, r ref.DoltRef) (hash.Hash, error) {
	c, err := ddb.ResolveCommitRef(ctx, r)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("resolve %s: %w", r, err)
	}
	return c.HashOf()
}
