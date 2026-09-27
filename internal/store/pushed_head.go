package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cenkalti/backoff/v4"
	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/types"
)

// RecordPushedHead records, on the store at doltRootDir, what a push run from
// a clone of it established at the peer: the remote-tracking ref
// `remotes/<remote>/<branch>` is set to head — the commit the clone pushed as
// HEAD:<branch> — exactly as a push from this store would have left it. Dolt's
// own push does this last step on the pushing store (SetHeadToCommit on the
// remote ref, env/actions/remotes.go); when the push runs from a clone, the
// clone gets that update and the live store does not, so this carries it back.
//
// It contacts no network and opens no SQL engine. The chunk store is opened
// directly through dolt's own loader with the same journal and lock semantics
// the engine uses (journaling on, singleton cache off, fail-fast on the journal
// lock so the wait is this package's bounded retry and not dolt's read-only
// fallback), the one ref is moved, and the store is closed — the whole hold
// is a few file reads and one journal append. Order of acquisition is the
// package's declared one: workspace shared, then Dolt's LOCK (taken by the
// open, waited out for engineOpenRetryMaxElapsed like any write open), then
// the commit lock for the write.
//
// Why it exists: the on-change mirror pushes from a clone
// (links-scale-om3r.s2h) so the live store is held for the clone step rather
// than the network round trip. Left alone, every freshness read on the live
// store — the staleness banner's "N local change(s) not pushed", `lit doctor`'s
// ahead count — would report pushed commits as unpushed until the next fetch,
// on every read command, for up to the receive interval. The tracking ref stays
// the one source of "where the remote is"; this is the push's own bookkeeping
// moved to the store that owns the ref. [LAW:one-source-of-truth]
//
// head must be a commit this store already holds — it was cloned from here —
// so dolt's SetHead refuses an address that is absent or not a commit, and
// that refusal surfaces as the error. [LAW:no-silent-failure]
func RecordPushedHead(ctx context.Context, doltRootDir string, remote string, branch string, head string) (err error) {
	root, err := validateDoltRootDir(doltRootDir)
	if err != nil {
		return err
	}
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return err
	}
	trimmedHead := strings.TrimSpace(head)
	if !isDoltCommitHash(trimmedHead) {
		return fmt.Errorf("record pushed head: %q is not a Dolt commit hash", head)
	}
	// The same shared hold every reader of the Dolt directory takes: a
	// rotator (snapshots restore, adopt) must not swap the directory under
	// the open below. [LAW:single-enforcer]
	releaseWorkspace, err := acquireWorkspaceShared(ctx, root)
	if err != nil {
		return err
	}
	defer func() {
		if relErr := releaseWorkspace(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	if err := requireInitializedWorkspace(root); err != nil {
		return err
	}
	if err := requireNoPendingAdopt(root); err != nil {
		return err
	}
	ddb, err := openChunkStoreForRefWrite(ctx, root)
	if err != nil {
		return err
	}
	defer func() {
		// Close is what releases Dolt's LOCK and flushes the manifest; a
		// failure there is the caller's to hear. [LAW:no-silent-failure]
		if closeErr := ddb.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	// Commit lock AFTER the open took LOCK — the package's order. It excludes
	// a writer mid-mutation whose GC-contention reconnect has momentarily
	// dropped LOCK (doc.go's tolerated inversion); LOCK alone would let this
	// ref write land inside that gap.
	releaseCommit, err := acquireCommitLockAtPath(ctx, workspaceStorageDir(root), commitLockPathForDolt(root))
	if err != nil {
		return err
	}
	defer func() {
		err = SettleCommitLockRelease(err, releaseCommit())
	}()
	if setErr := ddb.SetHead(ctx, ref.NewRemoteRef(trimmedRemote, trimmedBranch), hash.Parse(trimmedHead)); setErr != nil {
		return fmt.Errorf("record pushed head %s on remotes/%s/%s: %w", trimmedHead, trimmedRemote, trimmedBranch, setErr)
	}
	return nil
}

// openChunkStoreForRefWrite opens the workspace's Dolt database without a SQL
// engine, with the open semantics every lit write engine has: the chunk
// journal on (the store is journaled; a non-journaling open refuses it),
// dolt's in-process singleton cache off (so this handle's lifetime and its
// LOCK hold are one fact), and fail-fast on LOCK contention, which this
// package then retries under the same bounded backoff a write engine open
// uses — so a mirror or command holding the store is waited out for exactly
// engineOpenRetryMaxElapsed and no longer. [LAW:one-source-of-truth] the
// wait is the write open's wait, not a second figure.
func openChunkStoreForRefWrite(ctx context.Context, root string) (*doltdb.DoltDB, error) {
	nomsDir := filepath.Join(root, doltDatabaseName, dbfactory.DoltDataDir)
	urlStr := "file://" + filepath.ToSlash(nomsDir)
	var ddb *doltdb.DoltDB
	open := func() error {
		params := map[string]any{
			dbfactory.ChunkJournalParam:             struct{}{},
			dbfactory.DisableSingletonCacheParam:    struct{}{},
			dbfactory.FailOnJournalLockTimeoutParam: struct{}{},
		}
		opened, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_Default, urlStr, filesys.LocalFS, params)
		if err != nil {
			if errors.Is(err, nbs.ErrDatabaseLocked) {
				return err
			}
			return backoff.Permanent(err)
		}
		ddb = opened
		return nil
	}
	if err := backoff.Retry(open, backoff.WithContext(newEngineOpenBackOff(), ctx)); err != nil {
		return nil, wrapEngineOpenContention(fmt.Errorf("open dolt chunk store at %s: %w", nomsDir, err))
	}
	return ddb, nil
}
