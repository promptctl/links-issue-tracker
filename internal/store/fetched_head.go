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

// FetchedHeadRecord is what LandFetchedHead did to the live store's tracking
// ref. [LAW:types-are-the-program] all three are successes, and a caller
// tracing the receive must tell them apart.
type FetchedHeadRecord int

const (
	// FetchedHeadMoved: the ref now names the head the clone fetched.
	FetchedHeadMoved FetchedHeadRecord = iota + 1
	// FetchedHeadUnchanged: the ref already named the fetched head.
	FetchedHeadUnchanged
	// FetchedHeadAbsent: the clone's fetch found no such branch on the remote,
	// so there was nothing to land and the live ref was not touched.
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
// ref, and how long the live store was held for the copy.
type LandedFetch struct {
	Record FetchedHeadRecord
	// Held is the landing's work under the hold, from the open's success to
	// the end of the git copy; zero when nothing was landed.
	Held time.Duration
}

// LandFetchedHead carries a fetch that ran on a clone back to the live store
// at doltRootDir: every chunk the clone's `remotes/<remote>/<branch>` reaches
// is copied into the live store, the live ref is set to the same commit, and
// the git objects the fetch brought into the clone's mirror of the remote are
// copied into the live store's mirror. It contacts no network and opens no SQL
// engine; the copies are local.
//
// Why it exists: the automatic receive fetches from a clone, so the live store
// is held for this copy rather than for the network round trip. Dolt's own fetch writes the chunks, the ref and the
// mirror's objects into the store it runs on; this is those three writes moved
// to the store that owns them. [LAW:one-source-of-truth] the tracking ref
// stays the one source of "where the remote is".
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
// coResidentHolderWait fails a contender naming the receiving command, which
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
	fetched, found, err := resolveTrackingHead(ctx, clone, trackingRef)
	if err != nil {
		return LandedFetch{}, fmt.Errorf("read the clone's %s: %w", trackingRef.String(), err)
	}
	if !found {
		return LandedFetch{Record: FetchedHeadAbsent}, nil
	}
	// Which mirror refs to copy is decided before the hold: the listing is two
	// git subprocesses per mirror, and a ref the live mirror gains meanwhile
	// has a name no clone ref shares (Dolt names one per blobstore instance),
	// so the plan cannot go stale in a way that matters.
	cachePlan, cacheErr := planRemoteCacheLanding(ctx, remoteCacheBaseAt(root), remoteCacheBaseAt(cloneRoot))

	// The same shared hold every reader of the Dolt directory takes: a rotator
	// (snapshots restore, adopt) must not swap the directory under the open.
	// [LAW:single-enforcer]
	releaseWorkspace, err := acquireWorkspaceShared(ctx, root)
	if err != nil {
		return LandedFetch{}, err
	}
	defer func() {
		if relErr := releaseWorkspace(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	if err := requireInitializedWorkspace(root); err != nil {
		return LandedFetch{}, err
	}
	if err := requireNoPendingAdopt(root); err != nil {
		return LandedFetch{}, err
	}
	err = holdForRefWrite(ctx, root, InlineReceiveDeadline, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		start := time.Now()
		defer func() { landed.Held = time.Since(start) }()
		tempDir := filepath.Join(root, doltDatabaseName, dbfactory.DoltDir, "temptf")
		if err := os.MkdirAll(tempDir, 0o755); err != nil {
			return fmt.Errorf("create temp table file dir: %w", err)
		}
		if err := ddb.PullChunks(holdCtx, tempDir, clone, []hash.Hash{fetched}, nil, nil); err != nil {
			return fmt.Errorf("copy the fetched chunks of %s into the live store: %w", fetched, err)
		}
		var landErr error
		landed.Record, landErr = landTrackingRef(holdCtx, ddb, trackingRef, fetched)
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

// landTrackingRef sets ref to fetched unless it already names it — a fetch's
// own semantics, whatever the relation between the two (LandFetchedHead says
// why). It reads fetched as a commit first, so a copy that did not land it is
// refused rather than pointed at.
func landTrackingRef(ctx context.Context, ddb *doltdb.DoltDB, trackingRef ref.DoltRef, fetched hash.Hash) (FetchedHeadRecord, error) {
	value, err := ddb.ReadCommit(ctx, fetched)
	if err != nil {
		return 0, fmt.Errorf("land fetched head %s: the copied chunks do not hold it as a commit: %w", fetched, err)
	}
	if _, ok := value.ToCommit(); !ok {
		return 0, fmt.Errorf("land fetched head %s: the live store holds it only as a ghost commit", fetched)
	}
	current, found, err := resolveTrackingHead(ctx, ddb, trackingRef)
	if err != nil {
		return 0, fmt.Errorf("land fetched head %s: read %s: %w", fetched, trackingRef.String(), err)
	}
	if found && current == fetched {
		return FetchedHeadUnchanged, nil
	}
	if err := ddb.SetHead(ctx, trackingRef, fetched); err != nil {
		return 0, fmt.Errorf("land fetched head %s on %s: %w", fetched, trackingRef.String(), err)
	}
	return FetchedHeadMoved, nil
}

// resolveTrackingHead reads the commit a tracking ref names. An absent ref is
// a real answer — the remote has no such branch — not an error.
func resolveTrackingHead(ctx context.Context, ddb *doltdb.DoltDB, trackingRef ref.DoltRef) (hash.Hash, bool, error) {
	has, err := ddb.HasRef(ctx, trackingRef)
	if err != nil || !has {
		return hash.Hash{}, false, err
	}
	commit, err := ddb.ResolveCommitRef(ctx, trackingRef)
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

// remoteCacheLanding is what a fetch on the clone added to its mirrors of the
// remote, to be copied into the live store's mirrors. A clone starts with a
// copy of the live mirrors, so without this the live mirror would never learn
// what the clone fetched, and every later clone would fetch it again, a little
// more each time.
type remoteCacheLanding struct {
	// moves are mirrors the live store does not have yet, moved over whole.
	moves []mirrorMove
	// copies are mirrors both have, with the refs only the clone holds.
	copies []mirrorCopy
}

type mirrorMove struct{ from, to, liveBase string }

type mirrorCopy struct {
	liveRepo, cloneRepo string
	refspecs            []string
}

// planRemoteCacheLanding decides, for every mirror in the clone, whether it is
// moved over whole or which of its refs are copied. A mirror the live store
// has gets the refs the clone's fetch created — Dolt names one per blobstore
// instance, so they are the refs the clone holds and the live mirror does not.
// A ref both hold is left alone: the live mirror's value is at least as new as
// the copy the clone started from. A mirror with nothing new costs nothing
// under the hold. A mirror that cannot be listed is reported and skipped.
func planRemoteCacheLanding(ctx context.Context, liveBase, cloneBase string) (remoteCacheLanding, error) {
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
		cloneRefs, err := mirrorRefs(ctx, cloneRepo)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		liveRefs, err := mirrorRefs(ctx, liveRepo)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var refspecs []string
		for name := range cloneRefs {
			if _, held := liveRefs[name]; !held {
				refspecs = append(refspecs, name+":"+name)
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

// mirrorRefs lists the refs under refs/dolt/ in one mirror.
func mirrorRefs(ctx context.Context, gitDir string) (map[string]struct{}, error) {
	out, err := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "for-each-ref", "--format=%(refname)", "refs/dolt/").Output()
	if err != nil {
		return nil, fmt.Errorf("list the refs of mirror %s: %w", gitDir, err)
	}
	refs := map[string]struct{}{}
	for _, name := range strings.Fields(string(out)) {
		refs[name] = struct{}{}
	}
	return refs, nil
}
