package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltenv "github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/libraries/utils/gitcmd"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/types"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// FetchedHeadRecord is what LandFetchedHead did to the live store's tracking
// ref. [LAW:types-are-the-program] all three are successes, and a caller
// tracing the receive must tell them apart.
type FetchedHeadRecord int

const (
	// FetchedHeadMoved: the ref now names the clone's head for it.
	FetchedHeadMoved FetchedHeadRecord = iota + 1
	// FetchedHeadUnchanged: the ref already named the clone's head for it.
	FetchedHeadUnchanged
	// FetchedHeadAbsent: the clone's fetch found no such branch on the
	// remote, so there was nothing to land on the ref. A tracking ref the
	// fetch pruned goes by LandedFetch.Pruned, and a fork point left without
	// its ref by LandedFetch.ForkPointsDropped.
	FetchedHeadAbsent
)

func (r FetchedHeadRecord) String() string {
	switch r {
	case FetchedHeadMoved:
		return "moved"
	case FetchedHeadUnchanged:
		return "unchanged"
	case FetchedHeadAbsent:
		return "absent"
	}
	return "unlanded"
}

// ErrRemoteCacheNotLanded marks a LandFetchedHead whose chunks and ref landed
// and whose git objects did not reach the live store's mirror of the remote:
// the fetch stands, and what is lost is only that the next network operation
// on the live store downloads those objects again.
var ErrRemoteCacheNotLanded = errors.New("the fetched git objects did not reach the live store's mirror of the remote, so its next network operation downloads them again")

// LandedFetch is what LandFetchedHead established: what it did to the tracking
// ref and to its fork point, which of the remote's tracking refs and fork
// points it deleted, and how long the live store was held for the copy.
type LandedFetch struct {
	Record FetchedHeadRecord
	// ForkPoints is what the landing did to the tracking ref's fork point
	// before it moved the ref, by the rule a fetch records by
	// (recordForkPoint): one record, or none when the live store held no
	// tracking ref to move.
	ForkPoints []storage.ForkPointRecord
	// Pruned is every tracking ref the clone's fetch pruned that the live
	// store still held at the head the clone pruned, and so deleted.
	Pruned []storage.PrunedRef
	// ForkPointsDropped is every live fork point of the remote left without
	// its tracking ref once the landing's refs stood, and so deleted.
	ForkPointsDropped []storage.ForkPointRecord
	// Held is the landing's work under the hold, from the open's success to
	// the end of the git copy; zero when nothing was landed.
	Held time.Duration
}

// LandFetchedHead carries a fetch that ran on a clone back to the live store
// at doltRootDir: every chunk the clone's `remotes/<remote>/<branch>` reaches
// is copied into the live store, the live fork point is recorded and then the
// live ref is set to the clone's head, and the git objects the fetch brought
// into the clone's mirror of the remote are copied into the live store's
// mirror. It contacts no network and opens no SQL engine; the copies are
// local.
//
// Why it exists: the automatic receive fetches from a clone, so the live store
// is held for this copy rather than for the network round trip. A fetch on a
// store records the fork point (recordForkPoints), then writes the chunks, the
// ref and the mirror's objects; this is those writes moved to the store that
// owns them, in the same order. [LAW:one-source-of-truth] the tracking ref
// stays the one source of "where the remote is".
//
// The fork point is recorded on the live store, from the live local branch
// and the live tracking ref the landing is about to move, by the rule the
// fetch records by (recordForkPoint) — never copied from the clone. The
// clone's was decided against its snapshot of the local branch, and a follow
// of a re-root can land on the live store while the clone fetches: the
// follow moves the live fork point onto the re-rooted head and collects the
// history below the cut, and the clone's fork point is a commit of that
// history. [LAW:one-source-of-truth] The live fork point only ever names the
// live tracking ref's head, a commit the live store already holds, so
// recording it copies nothing.
//
// The ref is set to what the fetch found whenever it differs, forwards,
// backwards, or onto an unrelated history, because that is what a fetch does:
// it records where the remote was when it looked. The one case in which that
// is older than the ref is a push the mirror recorded after the clone's fetch
// read the remote; the ref then reads that push as unpushed until the next
// receive, which finds the remote's advertisement changed and fetches again.
// Refusing to move the ref instead would leave it wrong for good whenever the
// remote really was reset, since the receive would then record the reset
// advertisement as received and stop asking.
//
// The hold is holdForRefWrite's: the workspace shared lock around Dolt's LOCK
// and the commit lock. Its budget is the receive's own deadline, not the
// mirror's hold budget: the landing's cost is proportional to how far the
// remote moved — tens of milliseconds for the usual few commits, more after a
// long absence — and a budget sized for a ref write would cut a large landing
// on every attempt, so it would never converge. A landing that outlasts
// coResidentHolderWait fails a contender naming the receive worker, which
// is the ordinary account of a long hold.
//
// The clone's fetch pruned too, and pruned is what it pruned (its session's
// PrunedRefs). Each of remote's pruned refs the clone still lacks is deleted
// from the live store only while it still names the head the clone pruned.
// For a session whose first fetch pruned the ref, that head is the live one
// at the take, since the clone is a copy of the live store; a ref the live
// store moved or created since is left as it stands, and so is one an earlier
// fetch of the session moved before a later one pruned it, until the next
// receive's own fetch prunes it. Then every live
// fork point of remote whose tracking ref the live store no longer holds goes,
// by the one rule a fetch's own drop follows (orphanForkPoints). It runs after
// the sync branch's ref is set, so the sync branch keeps its fork point
// whenever the remote has the branch.
//
// The mirror's objects land last: a failure there is returned wrapping
// ErrRemoteCacheNotLanded with the result, because the chunks and ref it
// follows are already written and stand.
func LandFetchedHead(ctx context.Context, doltRootDir, cloneRootDir string, remote storage.RemoteName, branch string, pruned []storage.PrunedRef) (landed LandedFetch, err error) {
	root, err := validateDoltRootDir(doltRootDir)
	if err != nil {
		return LandedFetch{}, err
	}
	cloneRoot, err := validateDoltRootDir(cloneRootDir)
	if err != nil {
		return LandedFetch{}, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return LandedFetch{}, err
	}
	trackingRef := ref.NewRemoteRef(remote.String(), trimmedBranch)

	// The clone is this caller's alone — nothing else opens it — so it is read
	// before the live store is held, and stays open for the copy.
	clone, err := openCloneChunkStore(ctx, cloneRoot)
	if err != nil {
		return LandedFetch{}, err
	}
	defer func() { err = errors.Join(err, clone.Close()) }()
	fetched, found, err := resolveRefHead(ctx, clone, trackingRef)
	if err != nil {
		return LandedFetch{}, fmt.Errorf("read the clone's %s: %w", trackingRef.String(), err)
	}
	pruned, err = clonePrunes(ctx, clone, remote, pruned)
	if err != nil {
		return LandedFetch{}, err
	}
	var heads []hash.Hash
	if found {
		heads = append(heads, fetched)
	}
	// Which mirror refs to copy is decided before the hold: the listing is two
	// git subprocesses per mirror. The plan names only the fetched remote's
	// last-known refs, so a live ref that moves meanwhile is one a live
	// instance republished on close, and the plan going stale there costs
	// nothing: the copy sets the ref to the clone's value, the same
	// last-writer-wins rule every close follows.
	cachePlan, cacheErr := planRemoteCacheLanding(ctx, remoteCacheBaseAt(root), remoteCacheBaseAt(cloneRoot), remote)

	releaseWorkspace, err := acquireLiveStoreHold(ctx, root)
	if err != nil {
		return LandedFetch{}, err
	}
	defer func() { err = errors.Join(err, releaseWorkspace()) }()
	err = holdForRefWrite(ctx, root, ReceiveDeadline, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		start := time.Now()
		defer func() { landed.Held = time.Since(start) }()
		tempDir := LandingTempDir(root)
		if err := os.MkdirAll(tempDir, 0o755); err != nil {
			return fmt.Errorf("create temp table file dir: %w", err)
		}
		if err := ddb.PullChunks(holdCtx, tempDir, clone, heads, nil, nil); err != nil {
			return fmt.Errorf("copy the fetched chunks of %v into the live store: %w", heads, err)
		}
		var landErr error
		landed.ForkPoints, landErr = recordLiveForkPoint(holdCtx, ddb, root, ownerOf(remote), trackingRef)
		if landErr != nil {
			return landErr
		}
		landed.Record = FetchedHeadAbsent
		if found {
			landed.Record, landErr = landRef(holdCtx, ddb, trackingRef, fetched)
			if landErr != nil {
				return landErr
			}
		}
		landed.Pruned, landErr = landPrunes(holdCtx, ddb, pruned)
		if landErr != nil {
			return landErr
		}
		landed.ForkPointsDropped, landErr = dropOrphanForkPointsAt(holdCtx, ddb, ownerOf(remote))
		if landErr != nil {
			return landErr
		}
		cacheErr = errors.Join(cacheErr, cachePlan.apply(holdCtx))
		return nil
	})
	// A failed hold returns what it did before the failure: each deletion
	// stands once made. [LAW:nothing-unseen]
	if err != nil {
		return landed, err
	}
	if cacheErr != nil {
		return landed, fmt.Errorf("%w: %s landed (%s): %w", ErrRemoteCacheNotLanded, trackingRef.String(), landed.Record, cacheErr)
	}
	return landed, nil
}

// LandingTempDir is where LandFetchedHead stages the table files it copies
// into the live store at doltRootDir.
func LandingTempDir(doltRootDir string) string {
	return filepath.Join(doltRootDir, doltDatabaseName, dbfactory.DoltDir, "temptf")
}

// landRef sets the tracking ref r to fetched unless it already names it — a
// fetch's own semantics, whatever the relation between the two
// (LandFetchedHead says why). It reads fetched as a commit first, so a copy
// that did not land it is refused rather than pointed at.
func landRef(ctx context.Context, ddb *doltdb.DoltDB, r ref.DoltRef, fetched hash.Hash) (FetchedHeadRecord, error) {
	if _, err := readFullCommit(ctx, ddb, fetched); err != nil {
		return 0, fmt.Errorf("land fetched head %s: the copied chunks do not hold it as a commit: %w", fetched, err)
	}
	current, found, err := resolveRefHead(ctx, ddb, r)
	if err != nil {
		return 0, fmt.Errorf("land fetched head %s: read %s: %w", fetched, r.String(), err)
	}
	if found && current == fetched {
		return FetchedHeadUnchanged, nil
	}
	if err := ddb.SetHead(ctx, r, fetched); err != nil {
		return 0, fmt.Errorf("land fetched head %s on %s: %w", fetched, r.String(), err)
	}
	return FetchedHeadMoved, nil
}

// recordLiveForkPoint is recordForkPoints for the one tracking ref a landing
// moves, on the store at root held with no SQL engine: the fork point of
// trackingRef, which owner owns, is recorded against the head that ref names
// now, before the landing moves it. A store that holds no such ref has
// nothing to move off, and so no record. [LAW:one-source-of-truth] the ref is
// the one the landing moves, never rebuilt here.
func recordLiveForkPoint(ctx context.Context, ddb *doltdb.DoltDB, root string, owner refOwner, trackingRef ref.RemoteRef) ([]storage.ForkPointRecord, error) {
	head, found, err := resolveRefHead(ctx, ddb, trackingRef)
	if err != nil {
		return nil, fmt.Errorf("read the live %s: %w", trackingRef.String(), err)
	}
	if !found {
		return nil, nil
	}
	local, err := localBranchAt(root)
	if err != nil {
		return nil, err
	}
	localHead, err := ddb.ResolveCommitRef(ctx, local)
	if err != nil {
		return nil, fmt.Errorf("read the local branch %s: %w", local.String(), err)
	}
	recorded, err := refHeadsOfType(ctx, ddb, ref.BranchRefType, forkPointBranch(owner, ""))
	if err != nil {
		return nil, err
	}
	record, err := recordForkPoint(ctx, chunkForkPoints{ddb: ddb, local: localHead}, owner, trackingRef.GetBranch(), head.String(), recorded)
	if err != nil {
		return nil, err
	}
	return []storage.ForkPointRecord{record}, nil
}

// chunkForkPoints is a forkPointStore on a chunk store held with no SQL
// engine, whose local branch's head is local.
type chunkForkPoints struct {
	ddb   *doltdb.DoltDB
	local *doltdb.Commit
}

func (c chunkForkPoints) sharesHistory(ctx context.Context, head string) (bool, error) {
	commit, err := readFullCommit(ctx, c.ddb, hash.Parse(head))
	if err != nil {
		return false, fmt.Errorf("read %s: %w", head, err)
	}
	_, err = doltdb.GetCommitAncestor(ctx, c.local, commit)
	if errors.Is(err, doltdb.ErrNoCommonAncestor) {
		return false, nil
	}
	return err == nil, err
}

// setForkPoint moves the fork point branch with its working set, as
// DOLT_BRANCH -f does on an engine.
func (c chunkForkPoints) setForkPoint(ctx context.Context, name, head string) error {
	commit, err := readFullCommit(ctx, c.ddb, hash.Parse(head))
	if err != nil {
		return err
	}
	return c.ddb.NewBranchAtCommit(ctx, ref.NewBranchRef(name), commit, nil)
}

// localBranchAt is the local branch of the Dolt root at root: the head its
// repo state names, which is the branch every session opens on and
// active_branch() reports, and which DOLT_BRANCH -m moves when it renames
// that branch. It is how a store held with no SQL engine reads it.
func localBranchAt(root string) (ref.DoltRef, error) {
	fs, err := filesys.LocalFS.WithWorkingDir(filepath.Join(root, doltDatabaseName))
	if err != nil {
		return nil, fmt.Errorf("read the local branch of %s: %w", root, err)
	}
	state, err := doltenv.LoadRepoState(fs)
	if err != nil {
		return nil, fmt.Errorf("read the local branch of %s: %w", root, err)
	}
	return state.CWBHeadRef(), nil
}

// clonePrunes is the prunes of remote the clone's session ended on: those
// whose tracking ref the clone does not hold. A session fetches more than
// once, so a later fetch that brought a branch back undid its prune.
func clonePrunes(ctx context.Context, clone *doltdb.DoltDB, remote storage.RemoteName, pruned []storage.PrunedRef) ([]storage.PrunedRef, error) {
	var standing []storage.PrunedRef
	for _, p := range pruned {
		if p.Remote != remote.String() {
			continue
		}
		r := ref.NewRemoteRef(p.Remote, p.Branch)
		_, held, err := resolveRefHead(ctx, clone, r)
		if err != nil {
			return nil, fmt.Errorf("read the clone's %s: %w", r.String(), err)
		}
		if !held {
			standing = append(standing, p)
		}
	}
	return standing, nil
}

// landPrunes deletes each pruned tracking ref the live store still holds at
// the head the clone pruned, and returns those it deleted. A ref already
// gone, or moved since the take, is left as it stands.
func landPrunes(ctx context.Context, ddb *doltdb.DoltDB, pruned []storage.PrunedRef) ([]storage.PrunedRef, error) {
	var deleted []storage.PrunedRef
	for _, p := range pruned {
		r := ref.NewRemoteRef(p.Remote, p.Branch)
		current, found, err := resolveRefHead(ctx, ddb, r)
		if err != nil {
			return deleted, fmt.Errorf("land the prune of %s: read it: %w", r.String(), err)
		}
		if !found || current.String() != p.Head {
			continue
		}
		if err := deleteRef(ctx, ddb, r); err != nil {
			return deleted, fmt.Errorf("land the prune of %s: %w", r.String(), err)
		}
		deleted = append(deleted, p)
	}
	return deleted, nil
}

// dropOrphanForkPointsAt is dropForkPoints for a store held with no SQL engine:
// every fork point of owner whose tracking ref the store does not hold goes,
// decided by orphanForkPoints.
func dropOrphanForkPointsAt(ctx context.Context, ddb *doltdb.DoltDB, owner refOwner) ([]storage.ForkPointRecord, error) {
	tracked, err := refHeadsOfType(ctx, ddb, ref.RemoteRefType, trackingRefPrefix(owner))
	if err != nil {
		return nil, err
	}
	recorded, err := refHeadsOfType(ctx, ddb, ref.BranchRefType, forkPointBranch(owner, ""))
	if err != nil {
		return nil, err
	}
	orphans := orphanForkPoints(tracked, recorded)
	var dropped []storage.ForkPointRecord
	for _, branch := range slices.Sorted(maps.Keys(orphans)) {
		r := ref.NewBranchRef(forkPointBranch(owner, branch))
		if err := deleteRef(ctx, ddb, r); err != nil {
			return dropped, fmt.Errorf("drop fork point %s: %w", r.String(), err)
		}
		dropped = append(dropped, storage.ForkPointRecord{Remote: owner.String(), Branch: branch, Head: orphans[branch], Outcome: storage.ForkPointDropped})
	}
	return dropped, nil
}

// refHeadsOfType maps every ref of type t whose path, as refHeadsUnder's
// queries name it, begins with prefix, the prefix trimmed, to the commit it
// names.
func refHeadsOfType(ctx context.Context, ddb *doltdb.DoltDB, t ref.RefType, prefix string) (map[string]string, error) {
	heads := map[string]string{}
	err := ddb.VisitRefsOfType(ctx, map[ref.RefType]struct{}{t: {}}, func(r ref.DoltRef, head hash.Hash) error {
		name := r.GetPath()
		if t == ref.RemoteRefType {
			name = "remotes/" + name
		}
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			heads[rest] = head.String()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list refs under %s: %w", prefix, err)
	}
	return heads, nil
}

// deleteRef deletes r. A branch goes with its working set, as DOLT_BRANCH -D
// deletes it, so nothing keeps the branch's data reachable; any other ref is
// its head alone.
func deleteRef(ctx context.Context, ddb *doltdb.DoltDB, r ref.DoltRef) error {
	if ws, err := ref.WorkingSetRefForHead(r); err == nil {
		if err := ddb.DeleteWorkingSet(ctx, ws); err != nil {
			return fmt.Errorf("delete the working set of %s: %w", r.String(), err)
		}
	} else if !errors.Is(err, ref.ErrWorkingSetUnsupported) {
		return err
	}
	return ddb.DeleteBranch(ctx, r, nil)
}

// resolveRefHead reads the commit a ref names. An absent ref is a real answer
// — the remote has no such branch, the clone recorded no fork point — not an
// error.
func resolveRefHead(ctx context.Context, ddb *doltdb.DoltDB, r ref.DoltRef) (hash.Hash, bool, error) {
	has, err := ddb.HasRef(ctx, r)
	if err != nil || !has {
		return hash.Hash{}, false, err
	}
	commit, err := ddb.ResolveCommitRef(ctx, r)
	if err != nil {
		return hash.Hash{}, false, err
	}
	addr, err := commit.HashOf()
	if err != nil {
		return hash.Hash{}, false, err
	}
	return addr, true, nil
}

// openCloneChunkStore opens a clone's Dolt database without a SQL engine. The
// clone belongs to the one caller that made it, so there is no holder to wait
// out and no holder record to leave: the open is dolt's loader with the
// journal on and the singleton cache off, and nothing else.
func openCloneChunkStore(ctx context.Context, cloneRoot string) (*doltdb.DoltDB, error) {
	nomsDir := filepath.Join(cloneRoot, doltDatabaseName, dbfactory.DoltDataDir)
	params := map[string]any{
		dbfactory.ChunkJournalParam:          struct{}{},
		dbfactory.DisableSingletonCacheParam: struct{}{},
	}
	ddb, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_Default, "file://"+filepath.ToSlash(nomsDir), filesys.LocalFS, params)
	if err != nil {
		return nil, fmt.Errorf("open the clone's chunk store at %s: %w", nomsDir, err)
	}
	return ddb, nil
}

// remoteCacheBaseAt is the git-remote-cache directory of the Dolt root at
// root — Store.remoteCacheBase for a root with no Store open on it.
func remoteCacheBaseAt(root string) string {
	return filepath.Join(root, doltDatabaseName, dbfactory.DoltDir, remoteCacheDirName)
}

// remoteCacheLanding is what a fetch on the clone changed in its mirrors of the
// remote, to be copied into the live store's mirrors. A clone starts with a
// copy of the live mirrors, so without this the live mirror would never learn
// what the clone fetched, and every later clone would fetch it again, a little
// more each time.
type remoteCacheLanding struct {
	// moves are mirrors the live store does not have yet, moved over whole.
	moves []mirrorMove
	// copies are mirrors both have, with the fetched remote's last-known refs
	// the clone holds at a value the live mirror does not.
	copies []mirrorCopy
}

type mirrorMove struct{ from, to, liveBase string }

type mirrorCopy struct {
	liveRepo, cloneRepo string
	refspecs            []string
}

// planRemoteCacheLanding decides, for every mirror in the clone, whether it is
// moved over whole or which of its refs are copied. A mirror the live store
// has gets the clone's last-known refs of the fetched remote — what its
// fetch's blobstore published on close (fork patch 7) — wherever the live
// mirror lacks one or holds it at another value. Only last-known refs, because
// they are the only refs a mirror keeps between operations: any instance ref
// the clone holds is an orphan it copied from the live mirror when it was
// taken, which the live store may have swept since, and landing it would put
// back the snapshot the sweep released. Only the fetched remote's, because the
// fetch published nothing else: every other ref the clone holds is the value
// it copied from the live mirror, which carries no news and, if a live push
// has moved the ref since, would wind it back.
//
// A differing ref is set to the clone's value, for the reason LandFetchedHead
// sets the tracking ref: the value is where the remote was when the clone's
// fetch looked, and recording that is what a fetch does. The live value may be
// a later state — a live push closing after the clone's fetch — and is then
// replaced by an earlier one: a last-known ref is the base the next fetch
// negotiates from, not a record of the newest state, and any state the remote
// held serves, an earlier one costing that fetch the delta again. Git cannot
// tell which of two states is later, since the blobstore severs its history
// chain periodically. A ref both hold at the same value costs nothing under
// the hold. A mirror that cannot be listed is reported and skipped.
func planRemoteCacheLanding(ctx context.Context, liveBase string, cloneBase string, remote storage.RemoteName) (remoteCacheLanding, error) {
	fetched := lastKnownRefNamespace + remote.String() + "/"
	keys, err := listRemoteCacheKeys(cloneBase)
	if err != nil {
		return remoteCacheLanding{}, err
	}
	var plan remoteCacheLanding
	var errs []error
	for _, key := range keys {
		cloneRepo := filepath.Join(cloneBase, key, "repo.git")
		liveRepo := filepath.Join(liveBase, key, "repo.git")
		if _, err := os.Stat(liveRepo); errors.Is(err, fs.ErrNotExist) {
			plan.moves = append(plan.moves, mirrorMove{from: filepath.Join(cloneBase, key), to: filepath.Join(liveBase, key), liveBase: liveBase})
			continue
		} else if err != nil {
			errs = append(errs, fmt.Errorf("stat the live mirror %s: %w", key, err))
			continue
		}
		cloneRefs, err := refsUnder(ctx, cloneRepo, fetched)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		liveRefs, err := refsUnder(ctx, liveRepo, fetched)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var refspecs []string
		for name, object := range cloneRefs {
			if liveRefs[name] != object {
				// Forced, because a last-known ref moves to wherever the
				// remote was, which need not descend from where it had been.
				refspecs = append(refspecs, "+"+name+":"+name)
			}
		}
		if len(refspecs) > 0 {
			plan.copies = append(plan.copies, mirrorCopy{liveRepo: liveRepo, cloneRepo: cloneRepo, refspecs: refspecs})
		}
	}
	return plan, errors.Join(errs...)
}

// apply carries the plan out. It runs inside the live store's hold because
// every other writer of the live mirrors — Dolt's own fetch and push on the
// live store — runs under that store's LOCK too, so a mirror has one writer at
// a time.
func (p remoteCacheLanding) apply(ctx context.Context) error {
	var errs []error
	for _, m := range p.moves {
		if err := os.MkdirAll(m.liveBase, 0o755); err != nil {
			errs = append(errs, fmt.Errorf("create %s: %w", m.liveBase, err))
			continue
		}
		if err := os.Rename(m.from, m.to); err != nil {
			errs = append(errs, fmt.Errorf("move the clone's new mirror %s into the live store: %w", filepath.Base(m.to), err))
		}
	}
	for _, c := range p.copies {
		if out, err := mirrorFetch(ctx, c.liveRepo, c.cloneRepo, c.refspecs...).CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("copy %d ref(s) of the clone's mirror %s into the live store: %w: %s", len(c.refspecs), filepath.Base(filepath.Dir(c.liveRepo)), err, strings.TrimSpace(string(out))))
		}
	}
	return errors.Join(errs...)
}

// mirrorRef is one ref under refs/dolt/ in a mirror and the object it names.
type mirrorRef struct {
	name   string
	object string
}

// listMirrorRefs lists the refs under refs/dolt/ in one mirror, newest commit
// first. It is the one reading of a mirror's refs: the landing and the sweep
// both read through it. [LAW:one-source-of-truth]
func listMirrorRefs(ctx context.Context, gitDir string) ([]mirrorRef, error) {
	out, err := mirrorGit(ctx, gitDir, gitcmd.Spec{}, "for-each-ref",
		"--sort=-committerdate", "--format=%(refname) %(objectname)", "refs/dolt/").Output()
	if err != nil {
		return nil, fmt.Errorf("list the refs of mirror %s: %w", gitDir, err)
	}
	var refs []mirrorRef
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name, object, found := strings.Cut(line, " "); found {
			refs = append(refs, mirrorRef{name: name, object: object})
		}
	}
	return refs, nil
}

// refsUnder maps each ref under prefix in one mirror to the object it names.
func refsUnder(ctx context.Context, gitDir, prefix string) (map[string]string, error) {
	refs, err := listMirrorRefs(ctx, gitDir)
	if err != nil {
		return nil, err
	}
	under := map[string]string{}
	for _, ref := range refs {
		if strings.HasPrefix(ref.name, prefix) {
			under[ref.name] = ref.object
		}
	}
	return under, nil
}
