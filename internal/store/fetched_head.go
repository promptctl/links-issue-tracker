package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/types"
)

// FetchedHeadRecord is what LandFetchedHead did to one of the live store's
// refs: the tracking ref, or its fork point. [LAW:types-are-the-program] all
// three are successes, and a caller tracing the receive must tell them apart.
type FetchedHeadRecord int

const (
	// FetchedHeadMoved: the ref now names the clone's head for it.
	FetchedHeadMoved FetchedHeadRecord = iota + 1
	// FetchedHeadUnchanged: the ref already named the clone's head for it.
	FetchedHeadUnchanged
	// FetchedHeadAbsent: the clone held no such ref — for the tracking ref,
	// its fetch found no such branch on the remote; for the fork point, it
	// recorded none — so there was nothing to land and the live ref was not
	// touched.
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
// ref and to its fork point, and how long the live store was held for the copy.
type LandedFetch struct {
	Record FetchedHeadRecord
	// ForkPoint is what the landing did to the tracking ref's fork point:
	// absent when the clone recorded none.
	ForkPoint FetchedHeadRecord
	// Held is the landing's work under the hold, from the open's success to
	// the end of the git copy; zero when nothing was landed.
	Held time.Duration
}

// LandFetchedHead carries a fetch that ran on a clone back to the live store
// at doltRootDir: every chunk the clone's `remotes/<remote>/<branch>` and its
// fork point reach is copied into the live store, the live fork point and then
// the live ref are set to the same commits, and the git objects the fetch
// brought into the clone's mirror of the remote are copied into the live
// store's mirror. It contacts no network and opens no SQL
// engine; the copies are local.
//
// Why it exists: the automatic receive fetches from a clone, so the live store
// is held for this copy rather than for the network round trip. A fetch on a
// store writes the fork point (recordForkPoints), then the chunks, the ref and
// the mirror's objects; this is those writes moved to the store that owns
// them, in the same order, so the fork point never trails the ref it guards.
// [LAW:one-source-of-truth] the tracking ref stays the one source of "where
// the remote is".
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
// The mirror's objects land last: a failure there is returned wrapping
// ErrRemoteCacheNotLanded with the result, because the chunks and ref it
// follows are already written and stand.
func LandFetchedHead(ctx context.Context, doltRootDir, cloneRootDir, remote, branch string) (landed LandedFetch, err error) {
	root, err := validateDoltRootDir(doltRootDir)
	if err != nil {
		return LandedFetch{}, err
	}
	cloneRoot, err := validateDoltRootDir(cloneRootDir)
	if err != nil {
		return LandedFetch{}, err
	}
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return LandedFetch{}, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return LandedFetch{}, err
	}
	trackingRef := ref.NewRemoteRef(trimmedRemote, trimmedBranch)

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
	if !found {
		return LandedFetch{Record: FetchedHeadAbsent, ForkPoint: FetchedHeadAbsent}, nil
	}
	forkPointRef := ref.NewBranchRef(forkPointBranch(trimmedRemote, trimmedBranch))
	forkPoint, forkPointFound, err := resolveRefHead(ctx, clone, forkPointRef)
	if err != nil {
		return LandedFetch{}, fmt.Errorf("read the clone's %s: %w", forkPointRef.String(), err)
	}
	heads := []hash.Hash{fetched}
	if forkPointFound {
		heads = append(heads, forkPoint)
	}
	// Which mirror refs to copy is decided before the hold: the listing is two
	// git subprocesses per mirror. The plan names only the fetched remote's
	// last-known refs, so a live ref that moves meanwhile is one a live
	// instance republished on close, and the plan going stale there costs
	// nothing: the copy sets the ref to the clone's value, the same
	// last-writer-wins rule every close follows.
	cachePlan, cacheErr := planRemoteCacheLanding(ctx, remoteCacheBaseAt(root), remoteCacheBaseAt(cloneRoot), trimmedRemote)

	releaseWorkspace, err := acquireLiveStoreHold(ctx, root)
	if err != nil {
		return LandedFetch{}, err
	}
	defer func() { err = errors.Join(err, releaseWorkspace()) }()
	err = holdForRefWrite(ctx, root, ReceiveDeadline, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		start := time.Now()
		defer func() { landed.Held = time.Since(start) }()
		tempDir := filepath.Join(root, doltDatabaseName, dbfactory.DoltDir, "temptf")
		if err := os.MkdirAll(tempDir, 0o755); err != nil {
			return fmt.Errorf("create temp table file dir: %w", err)
		}
		if err := ddb.PullChunks(holdCtx, tempDir, clone, heads, nil, nil); err != nil {
			return fmt.Errorf("copy the fetched chunks of %v into the live store: %w", heads, err)
		}
		var landErr error
		landed.ForkPoint = FetchedHeadAbsent
		if forkPointFound {
			landed.ForkPoint, landErr = landRef(holdCtx, ddb, forkPointRef, forkPoint)
			if landErr != nil {
				return landErr
			}
		}
		landed.Record, landErr = landRef(holdCtx, ddb, trackingRef, fetched)
		if landErr != nil {
			return landErr
		}
		cacheErr = errors.Join(cacheErr, cachePlan.apply(holdCtx))
		return nil
	})
	if err != nil {
		return LandedFetch{Held: landed.Held}, err
	}
	if cacheErr != nil {
		return landed, fmt.Errorf("%w: %s landed (%s): %w", ErrRemoteCacheNotLanded, trackingRef.String(), landed.Record, cacheErr)
	}
	return landed, nil
}

// landRef sets r to fetched unless it already names it — a fetch's own
// semantics, whatever the relation between the two (LandFetchedHead says why).
// It reads fetched as a commit first, so a copy that did not land it is
// refused rather than pointed at. A branch moves with its working set, as
// DOLT_BRANCH -f moves the fork point a fetch on the store records; any other
// ref is its head alone.
func landRef(ctx context.Context, ddb *doltdb.DoltDB, r ref.DoltRef, fetched hash.Hash) (FetchedHeadRecord, error) {
	commit, err := readFullCommit(ctx, ddb, fetched)
	if err != nil {
		return 0, fmt.Errorf("land fetched head %s: the copied chunks do not hold it as a commit: %w", fetched, err)
	}
	current, found, err := resolveRefHead(ctx, ddb, r)
	if err != nil {
		return 0, fmt.Errorf("land fetched head %s: read %s: %w", fetched, r.String(), err)
	}
	if found && current == fetched {
		return FetchedHeadUnchanged, nil
	}
	if r.GetType() == ref.BranchRefType {
		err = ddb.NewBranchAtCommit(ctx, r, commit, nil)
	} else {
		err = ddb.SetHead(ctx, r, fetched)
	}
	if err != nil {
		return 0, fmt.Errorf("land fetched head %s on %s: %w", fetched, r.String(), err)
	}
	return FetchedHeadMoved, nil
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
func planRemoteCacheLanding(ctx context.Context, liveBase, cloneBase, remote string) (remoteCacheLanding, error) {
	fetched := lastKnownRefNamespace + remote + "/"
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
		args := append([]string{"--git-dir", c.liveRepo, "fetch", "--no-tags", "--quiet", c.cloneRepo}, c.refspecs...)
		if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
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
	out, err := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "for-each-ref",
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
