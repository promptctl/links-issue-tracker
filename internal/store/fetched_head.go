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
	// FetchedHeadCarried: the ref already named the fetched head or a
	// descendant of it — the mirror recorded a later push while the clone was
	// fetching — so the ref was left where it was.
	FetchedHeadCarried
	// FetchedHeadAbsent: the clone's fetch found no such branch on the remote,
	// so there was nothing to land and the live ref was not touched.
	FetchedHeadAbsent
)

func (r FetchedHeadRecord) String() string {
	switch r {
	case FetchedHeadMoved:
		return "moved"
	case FetchedHeadCarried:
		return "carried"
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

// LandFetchedHead carries a fetch that ran on a clone back to the live store
// at doltRootDir: every chunk the clone's `remotes/<remote>/<branch>` reaches
// is copied into the live store, the live ref is set to the same commit, and
// the git objects the fetch brought into the clone's mirror of the remote are
// copied into the live store's mirror. It contacts no network and opens no SQL
// engine; the copies are local.
//
// Why it exists: the automatic receive fetches from a clone
// (links-scale-t4vj), so the live store is held for this copy — tens of
// milliseconds — rather than for the network round trip, which is seconds.
// Dolt's own fetch writes the chunks, the ref and the mirror's objects into
// the store it runs on; this is those three writes moved to the store that
// owns them. [LAW:one-source-of-truth] the tracking ref stays the one source
// of "where the remote is".
//
// The hold is holdForRefWrite's: the workspace shared lock around Dolt's LOCK
// and the commit lock, the work under MirrorHoldBudget, a cut wrapping
// ErrMirrorHoldCut.
//
// The ref follows the fetch, except backwards. A fetch force-updates its
// tracking ref, so a remote rewritten under this store is recorded as the
// fetch found it. But the mirror's pushed-head record can land between the
// clone's fetch and this copy, carrying the ref past what the clone saw; the
// ref is then left where it is and the record reports FetchedHeadCarried,
// because the remote advertised that later commit after the fetch did.
//
// The mirror's objects land last: a failure there is returned wrapping
// ErrRemoteCacheNotLanded with the record, because the chunks and ref it
// follows are already written and stand.
func LandFetchedHead(ctx context.Context, doltRootDir, cloneRootDir, remote, branch string) (record FetchedHeadRecord, err error) {
	root, err := validateDoltRootDir(doltRootDir)
	if err != nil {
		return 0, err
	}
	cloneRoot, err := validateDoltRootDir(cloneRootDir)
	if err != nil {
		return 0, err
	}
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return 0, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return 0, err
	}
	trackingRef := ref.NewRemoteRef(trimmedRemote, trimmedBranch)

	// The clone is this caller's alone — nothing else opens it — so it is read
	// before the live store is held, and stays open for the copy.
	clone, err := openCloneChunkStore(ctx, cloneRoot)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, clone.Close()) }()
	fetched, found, err := resolveTrackingHead(ctx, clone, trackingRef)
	if err != nil {
		return 0, fmt.Errorf("read the clone's %s: %w", trackingRef.String(), err)
	}
	if !found {
		return FetchedHeadAbsent, nil
	}

	// The same shared hold every reader of the Dolt directory takes: a rotator
	// (snapshots restore, adopt) must not swap the directory under the open.
	// [LAW:single-enforcer]
	releaseWorkspace, err := acquireWorkspaceShared(ctx, root)
	if err != nil {
		return 0, err
	}
	defer func() {
		if relErr := releaseWorkspace(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	if err := requireInitializedWorkspace(root); err != nil {
		return 0, err
	}
	if err := requireNoPendingAdopt(root); err != nil {
		return 0, err
	}
	var cacheErr error
	err = holdForRefWrite(ctx, root, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		tempDir := filepath.Join(root, doltDatabaseName, dbfactory.DoltDir, "temptf")
		if err := os.MkdirAll(tempDir, 0o755); err != nil {
			return fmt.Errorf("create temp table file dir: %w", err)
		}
		if err := ddb.PullChunks(holdCtx, tempDir, clone, []hash.Hash{fetched}, nil, nil); err != nil {
			return fmt.Errorf("copy the fetched chunks of %s into the live store: %w", fetched, err)
		}
		var landErr error
		record, landErr = landTrackingRef(holdCtx, ddb, trackingRef, fetched)
		if landErr != nil {
			return landErr
		}
		cacheErr = landRemoteCache(holdCtx, remoteCacheBaseAt(root), remoteCacheBaseAt(cloneRoot))
		return nil
	})
	if err != nil {
		return 0, err
	}
	if cacheErr != nil {
		return record, fmt.Errorf("%w: %s landed (%s): %w", ErrRemoteCacheNotLanded, trackingRef.String(), record, cacheErr)
	}
	return record, nil
}

// landTrackingRef sets ref to fetched unless ref already names fetched or a
// descendant of it. CanFastForward's four answers are RecordPushedHead's;
// here a divergence is a rewritten remote and the fetch's answer wins, where
// a push's record refuses it.
func landTrackingRef(ctx context.Context, ddb *doltdb.DoltDB, trackingRef ref.DoltRef, fetched hash.Hash) (FetchedHeadRecord, error) {
	value, err := ddb.ReadCommit(ctx, fetched)
	if err != nil {
		return 0, fmt.Errorf("land fetched head %s: the copied chunks do not hold it as a commit: %w", fetched, err)
	}
	commit, ok := value.ToCommit()
	if !ok {
		return 0, fmt.Errorf("land fetched head %s: the live store holds it only as a ghost commit", fetched)
	}
	_, ffErr := ddb.CanFastForward(ctx, trackingRef, commit)
	switch {
	case errors.Is(ffErr, doltdb.ErrUpToDate), errors.Is(ffErr, doltdb.ErrIsAhead):
		return FetchedHeadCarried, nil
	case ffErr != nil:
		return 0, fmt.Errorf("land fetched head %s on %s: compare the ref with the fetched head: %w", fetched, trackingRef.String(), ffErr)
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

// landRemoteCache copies what a fetch on the clone added to its mirrors of the
// remote into the live store's mirrors. A clone starts with a copy of the live
// mirrors, so without this the live mirror would never learn what the clone
// fetched, and every later clone would fetch it again, a little more each
// time. A mirror the live store does not have yet is moved over whole. A
// mirror it has gets the refs the clone's fetch created — Dolt names one per
// blobstore instance, so they are the refs the clone holds and the live mirror
// does not — and the objects behind them, through one local `git fetch` that
// transfers only what the live mirror lacks. A ref both sides hold is left
// alone: the live mirror's value is at least as new as the copy the clone
// started from.
//
// It runs inside the live store's hold because every other writer of the live
// mirrors — Dolt's own fetch and push on the live store — runs under that
// store's LOCK too, so the mirror has one writer at a time.
func landRemoteCache(ctx context.Context, liveBase, cloneBase string) error {
	keys, err := listRemoteCacheKeys(cloneBase)
	if err != nil {
		return err
	}
	var errs []error
	for _, key := range keys {
		errs = append(errs, landRemoteMirror(ctx, liveBase, cloneBase, key))
	}
	return errors.Join(errs...)
}

// landRemoteMirror is landRemoteCache for one mirror.
func landRemoteMirror(ctx context.Context, liveBase, cloneBase, key string) error {
	cloneRepo := filepath.Join(cloneBase, key, "repo.git")
	liveRepo := filepath.Join(liveBase, key, "repo.git")
	if _, err := os.Stat(liveRepo); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(liveBase, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", liveBase, err)
		}
		if err := os.Rename(filepath.Join(cloneBase, key), filepath.Join(liveBase, key)); err != nil {
			return fmt.Errorf("move the clone's new mirror %s into the live store: %w", key, err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("stat the live mirror %s: %w", key, err)
	}
	cloneRefs, err := mirrorRefs(ctx, cloneRepo)
	if err != nil {
		return err
	}
	liveRefs, err := mirrorRefs(ctx, liveRepo)
	if err != nil {
		return err
	}
	var refspecs []string
	for name := range cloneRefs {
		if _, held := liveRefs[name]; !held {
			refspecs = append(refspecs, name+":"+name)
		}
	}
	if len(refspecs) == 0 {
		return nil
	}
	args := append([]string{"--git-dir", liveRepo, "fetch", "--no-tags", "--quiet", cloneRepo}, refspecs...)
	if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("copy %d ref(s) of the clone's mirror %s into the live store: %w: %s", len(refspecs), key, err, strings.TrimSpace(string(out)))
	}
	return nil
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
