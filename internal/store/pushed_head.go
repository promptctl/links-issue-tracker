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

// PushedHeadRecord is what RecordPushedHead did to the tracking ref: moved it
// to the pushed head, or found it already carrying that head (or a descendant
// of it) and left it alone. [LAW:types-are-the-program] the two outcomes are
// both successes and a caller logging the cycle must tell them apart.
type PushedHeadRecord int

const (
	// PushedHeadMoved: the ref now names the pushed head.
	PushedHeadMoved PushedHeadRecord = iota + 1
	// PushedHeadCarried: the ref already named the pushed head or a descendant
	// of it — a push or a fetch on the live store landed while the clone's
	// push was in flight — so the ref was left where it was. A record is a
	// push's bookkeeping arriving late; it never moves the ref backwards.
	PushedHeadCarried
)

func (r PushedHeadRecord) String() string {
	switch r {
	case PushedHeadMoved:
		return "moved"
	case PushedHeadCarried:
		return "carried"
	}
	return "unrecorded"
}

// ErrMirrorHoldCut marks a hold on the live store that ran past
// MirrorHoldBudget and was cut. It is the discriminator the mirror reads to
// say "the budget, not the work" in its durable trail; the wording of that
// explanation is the mirror's. [LAW:one-source-of-truth]
var ErrMirrorHoldCut = errors.New("mirror hold cut at its budget")

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
// The hold — everything from the open's success to the ref write — runs under
// MirrorHoldBudget, the same deadline the clone step runs under: the lock
// waits before it are waiting, not holding, and a ref write still running at
// the budget is a stalled one. A cut is returned wrapping ErrMirrorHoldCut.
// [LAW:no-ambient-temporal-coupling]
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
// The ref only ever moves forward. Nothing serializes the clone's push against
// the live store's own network traffic — an explicit `lit sync push` or a
// read command's inline receive runs while the clone's push is in flight — so
// by the time this runs the ref may already name a descendant of head: the
// explicit push landed a later commit, or a fetch brought a peer's. Setting
// it back to head would have every freshness read report that later commit
// as unpushed, the exact false report this record exists to remove. So the
// ref is compared first: at head or past it, it is left alone and the record
// reports PushedHeadCarried. A ref that has diverged from head — the remote
// was rewritten under the clone — is refused rather than guessed at; the next
// fetch settles it. [LAW:no-silent-failure]
//
// head must be a commit this store already holds — it was cloned from here —
// so a hash the store does not hold is refused, and that refusal surfaces as
// the error. [LAW:no-silent-failure]
func RecordPushedHead(ctx context.Context, doltRootDir string, remote string, branch string, head string) (record PushedHeadRecord, err error) {
	root, err := validateDoltRootDir(doltRootDir)
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
	trimmedHead := strings.TrimSpace(head)
	if !isDoltCommitHash(trimmedHead) {
		return 0, fmt.Errorf("record pushed head: %q is not a Dolt commit hash", head)
	}
	// The same shared hold every reader of the Dolt directory takes: a
	// rotator (snapshots restore, adopt) must not swap the directory under
	// the open below. [LAW:single-enforcer]
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
	ddb, err := openChunkStoreForRefWrite(ctx, root)
	if err != nil {
		return 0, err
	}
	defer func() {
		// Close is what releases Dolt's LOCK and flushes the manifest; a
		// failure there is the caller's to hear. [LAW:no-silent-failure]
		if closeErr := ddb.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	// The hold starts here: the open above took LOCK. Registered after the
	// Close defer so it runs first and the cut is stamped on the error the
	// hold produced, not on a Close failure after it.
	holdCtx, cancelHold := context.WithTimeout(ctx, MirrorHoldBudget)
	defer cancelHold()
	defer func() {
		if err != nil && holdCtx.Err() != nil && ctx.Err() == nil {
			err = fmt.Errorf("%w: %w", ErrMirrorHoldCut, err)
		}
	}()
	// Commit lock AFTER the open took LOCK — the package's order. It excludes
	// a writer mid-mutation whose GC-contention reconnect has momentarily
	// dropped LOCK (doc.go's tolerated inversion); LOCK alone would let this
	// ref write land inside that gap.
	releaseCommit, err := acquireCommitLockAtPath(holdCtx, workspaceStorageDir(root), commitLockPathForDolt(root))
	if err != nil {
		return 0, err
	}
	defer func() {
		err = SettleCommitLockRelease(err, releaseCommit())
	}()
	trackingRef := ref.NewRemoteRef(trimmedRemote, trimmedBranch)
	pushed, err := ddb.ReadCommit(holdCtx, hash.Parse(trimmedHead))
	if err != nil {
		return 0, fmt.Errorf("record pushed head %s: this store does not hold it as a commit: %w", trimmedHead, err)
	}
	pushedCommit, ok := pushed.ToCommit()
	if !ok {
		return 0, fmt.Errorf("record pushed head %s: this store holds it only as a ghost commit", trimmedHead)
	}
	// CanFastForward answers four ways for an existing ref: (true, nil) when
	// the ref is a strict ancestor of head, ErrUpToDate when it IS head,
	// ErrIsAhead when head is a strict ancestor of the ref, and (false, nil)
	// when the two share an ancestor that is neither — divergence. Any other
	// error is the comparison itself failing (a cut hold, an I/O fault, no
	// common ancestor) and says nothing about where the ref stands. An absent
	// ref answers (true, nil) — the first push.
	canMove, ffErr := ddb.CanFastForward(holdCtx, trackingRef, pushedCommit)
	switch {
	case errors.Is(ffErr, doltdb.ErrUpToDate), errors.Is(ffErr, doltdb.ErrIsAhead):
		return PushedHeadCarried, nil
	case ffErr != nil:
		return 0, fmt.Errorf("record pushed head %s on remotes/%s/%s: compare the ref with the pushed head: %w", trimmedHead, trimmedRemote, trimmedBranch, ffErr)
	case !canMove:
		return 0, fmt.Errorf("record pushed head %s on remotes/%s/%s: the ref has diverged from the pushed head (the remote was rewritten under the clone); the next fetch settles it", trimmedHead, trimmedRemote, trimmedBranch)
	}
	if setErr := ddb.SetHead(holdCtx, trackingRef, hash.Parse(trimmedHead)); setErr != nil {
		return 0, fmt.Errorf("record pushed head %s on remotes/%s/%s: %w", trimmedHead, trimmedRemote, trimmedBranch, setErr)
	}
	return PushedHeadMoved, nil
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
