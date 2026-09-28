package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

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

// ErrReceivedRefsNotRecorded marks a RecordPushedHead whose ref write landed
// and whose received-refs write did not: the push's bookkeeping stands, and
// what is lost is only the next automatic receive's skip.
var ErrReceivedRefsNotRecorded = errors.New("received-refs record not written, so the next automatic receive fetches")

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
// open, waited out for coResidentHolderWait like any write open), then
// the commit lock for the write.
//
// The hold — everything from the open's success to the ref write — runs under
// MirrorHoldBudget, the same deadline the clone step runs under: the lock
// waits before it are waiting, not holding, and a ref write still running at
// the budget is a stalled one. A cut is returned wrapping ErrMirrorHoldCut.
// [LAW:no-ambient-temporal-coupling]
//
// Why it exists: the on-change mirror pushes from a clone so the live store is
// held for the clone step rather than the network round trip. Left alone,
// every freshness read on the live store — the staleness banner's "N local
// change(s) not pushed", `lit doctor`'s ahead count — would report pushed
// commits as unpushed until the next fetch, on every read command, for up to
// the receive interval. The tracking ref stays the one source of "where the
// remote is"; this is the push's own bookkeeping moved to the store that owns
// the ref. [LAW:one-source-of-truth]
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
//
// receivedRefs, when non-nil, is the received-refs record the push proved: the
// advertisement it left the remote showing (internal/cli/sync_receive_ask.go
// owns its bytes and the proof). It is written here, inside the workspace
// hold and after the ref write has closed cleanly, for two reasons. The hold
// is what keeps a rotation of the Dolt directory from landing between the
// push's bookkeeping and the record, which would let the record describe a
// directory that was swapped out. And the record says "this store holds what
// the remote advertises", so the next receive skips its fetch: written over a
// ref that failed to move or failed to flush, it would stop that fetch from
// ever repairing the ref. It is written only when the ref ends at head; a ref
// already past head was carried there by something that recorded a newer
// advertisement. A nil record leaves the one on disk standing.
// [LAW:one-source-of-truth]
func RecordPushedHead(ctx context.Context, doltRootDir string, remote string, branch string, head string, receivedRefs []byte) (record PushedHeadRecord, err error) {
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
	record, atHead, err := settlePushedHead(ctx, root, trimmedRemote, trimmedBranch, trimmedHead)
	if err != nil {
		return 0, err
	}
	// The record is written only once the ref write has closed cleanly, and
	// only when the ref ends at the pushed head: a ref already past it was
	// carried there by a later push or fetch, which recorded something newer
	// than this push's advertisement. [LAW:dataflow-not-control-flow] exception:
	// a nil record, or a ref past head, leaves the record on disk standing.
	if receivedRefs != nil && atHead {
		if writeErr := WriteReceivedRefs(root, receivedRefs); writeErr != nil {
			return record, fmt.Errorf("%w: pushed head %s recorded (%s): %w", ErrReceivedRefsNotRecorded, trimmedHead, record, writeErr)
		}
	}
	return record, nil
}

// settlePushedHead is RecordPushedHead's ref write: the one ref comparison and
// move, run inside holdForRefWrite. atHead reports whether the ref now names
// head, moved there or already there, as against left on a descendant of it.
func settlePushedHead(ctx context.Context, root, trimmedRemote, trimmedBranch, trimmedHead string) (record PushedHeadRecord, atHead bool, err error) {
	err = holdForRefWrite(ctx, root, MirrorHoldBudget, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		trackingRef := ref.NewRemoteRef(trimmedRemote, trimmedBranch)
		pushed, err := ddb.ReadCommit(holdCtx, hash.Parse(trimmedHead))
		if err != nil {
			return fmt.Errorf("record pushed head %s: this store does not hold it as a commit: %w", trimmedHead, err)
		}
		pushedCommit, ok := pushed.ToCommit()
		if !ok {
			return fmt.Errorf("record pushed head %s: this store holds it only as a ghost commit", trimmedHead)
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
		case errors.Is(ffErr, doltdb.ErrUpToDate):
			record, atHead = PushedHeadCarried, true
			return nil
		case errors.Is(ffErr, doltdb.ErrIsAhead):
			record, atHead = PushedHeadCarried, false
			return nil
		case ffErr != nil:
			return fmt.Errorf("record pushed head %s on remotes/%s/%s: compare the ref with the pushed head: %w", trimmedHead, trimmedRemote, trimmedBranch, ffErr)
		case !canMove:
			return fmt.Errorf("record pushed head %s on remotes/%s/%s: the ref has diverged from the pushed head (the remote was rewritten under the clone); the next fetch settles it", trimmedHead, trimmedRemote, trimmedBranch)
		}
		if setErr := ddb.SetHead(holdCtx, trackingRef, hash.Parse(trimmedHead)); setErr != nil {
			return fmt.Errorf("record pushed head %s on remotes/%s/%s: %w", trimmedHead, trimmedRemote, trimmedBranch, setErr)
		}
		record, atHead = PushedHeadMoved, true
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return record, atHead, nil
}

// holdForRefWrite is the one hold a clone's bookkeeping takes on the live
// store: the chunk-store open (Dolt's LOCK, waited out like any write open),
// the commit lock, and write, all released before it returns. Both the mirror's
// pushed-head record and the receive's landing of a fetch run through it, so the
// two cannot drift on the lock order. [LAW:single-enforcer]
//
// The hold — everything from the open's success to the end of write — runs
// under budget: the lock waits before it are waiting, not holding. A cut is
// returned wrapping ErrMirrorHoldCut. What the budget is sized against is the
// caller's to say, because the two writes' costs differ in kind: a ref write is
// one journal append, a landing copies whatever the remote moved. The caller
// holds the workspace shared lock around it, so a rotation of the Dolt
// directory cannot land inside the hold.
func holdForRefWrite(ctx context.Context, root string, budget time.Duration, write func(holdCtx context.Context, ddb *doltdb.DoltDB) error) (err error) {
	ddb, releaseRecord, err := openChunkStoreForRefWrite(ctx, root)
	if err != nil {
		return err
	}
	defer func() {
		// The record retires first, then Close releases Dolt's LOCK and
		// flushes the manifest; a failure in either is the caller's to hear.
		// [LAW:no-silent-failure]
		err = errors.Join(err, releaseRecord(), ddb.Close())
	}()
	// The hold starts here: the open above took LOCK. Registered after the
	// Close defer so it runs first and the cut is stamped on the error the
	// hold produced, not on a Close failure after it.
	holdCtx, cancelHold := context.WithTimeout(ctx, budget)
	defer cancelHold()
	defer func() {
		if err != nil && holdCtx.Err() != nil && ctx.Err() == nil {
			err = fmt.Errorf("%w: %w", ErrMirrorHoldCut, err)
		}
	}()
	// Commit lock AFTER the open took LOCK — the package's order. It excludes
	// a writer mid-mutation whose GC-contention reconnect has momentarily
	// dropped LOCK (doc.go's tolerated inversion); LOCK alone would let this
	// write land inside that gap.
	releaseCommit, err := acquireCommitLockAtPath(holdCtx, workspaceStorageDir(root), commitLockPathForDolt(root))
	if err != nil {
		return err
	}
	defer func() {
		err = SettleCommitLockRelease(err, releaseCommit())
	}()
	return write(holdCtx, ddb)
}

// openChunkStoreForRefWrite opens the workspace's Dolt database without a SQL
// engine, with the open semantics every lit write engine has: the chunk
// journal on (the store is journaled; a non-journaling open refuses it),
// dolt's in-process singleton cache off (so this handle's lifetime and its
// LOCK hold are one fact), and fail-fast on LOCK contention, which this
// package then retries under the same wait a write engine open uses — so a
// mirror or command holding the store is waited out for exactly
// coResidentHolderWait and no longer, and a holder that outlasts it is named.
// [LAW:one-source-of-truth] the wait is the write open's wait, not a second
// figure. The open records this process as LOCK's holder for the life of
// the handle (recordLockHolder, wrapping a no-op release since the hold
// itself ends when the handle closes), exactly as a write engine does; the
// returned release retires the record and must run before the handle closes.
func openChunkStoreForRefWrite(ctx context.Context, root string) (*doltdb.DoltDB, func() error, error) {
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
	if err := awaitEngineOpen(ctx, root, func(ctx context.Context) error {
		return backoff.Retry(open, backoff.WithContext(newEngineOpenBackOff(root), ctx))
	}); err != nil {
		return nil, nil, fmt.Errorf("open dolt chunk store at %s: %w", nomsDir, err)
	}
	return ddb, recordLockHolder(workspaceStorageDir(root), DoltJournalLockPath(root), func() error { return nil }), nil
}
