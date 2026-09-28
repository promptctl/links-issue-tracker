package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// receiveCloneDirName is the directory under the workspace storage dir that
// holds the automatic receive's clone of the Dolt directory while its fetch
// runs: <StorageDir>/receive-clone/<stamp>/dolt. [LAW:one-source-of-truth]
const receiveCloneDirName = "receive-clone"

// receiveCloneBase is the parent of every receive's clone.
func receiveCloneBase(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, receiveCloneDirName)
}

// receiveFetch is what the fetch on the clone established: the sync target it
// resolved there, and the fetch's own failure when it ran and failed.
// [LAW:types-are-the-program] a could-not-attempt failure is the error beside
// it, never this field.
type receiveFetch struct {
	target   syncTarget
	fetchErr error
}

// takeReceiveClone clones the live store for the receive's fetch. The caller
// holds the receive lock, which is what makes cloneLiveStore's sweep of a
// dead receive's clone safe. The live store is held for the copy alone
// (links-scale-t4vj): the fetch that used to run under its LOCK — seconds on
// the network — runs from the clone with none of the live store's locks held.
func takeReceiveClone(ctx context.Context, ws workspace.Info) (liveClone, error) {
	clone, cut, err := cloneLiveStore(ctx, ws, receiveCloneBase(ws), func() {})
	if err != nil {
		// The hold's cost travels with the failure, as the mirror's does: a
		// cut read without it cannot tell a stalled copy from a slow one.
		err = fmt.Errorf("take the receive's clone (live store held %s): %w", clone.held.Round(time.Millisecond), err)
		if cut {
			err = fmt.Errorf("%w: %w", receiveCloneCutExplanation(), err)
		}
		return liveClone{}, err
	}
	return clone, nil
}

// removeReceiveClone discards the receive's clone. A failure is loud and never
// fatal: the receive it served is over, and the next receive's sweep collects
// the tree. [LAW:no-silent-failure]
func removeReceiveClone(clone liveClone) {
	if err := os.RemoveAll(clone.dir); err != nil {
		fmt.Fprintf(os.Stderr, "lit: automatic receive clone not removed (%v); the next receive collects it\n", err)
	}
}

// fetchIntoClone resolves the sync target on the clone and fetches the remote
// into it. The target is resolved there, not on the live store, because its
// remote checks go to the network and the live store is not held across the
// network; the clone holds the live store's remotes as of the take. The
// returned error is a could-not-attempt failure (the open, the target); a
// fetch that ran and failed is receiveFetch.fetchErr.
func fetchIntoClone(ctx context.Context, ws workspace.Info, clone liveClone) (fetch receiveFetch, err error) {
	session, closeClone, err := openSyncSessionAt(ctx, clone.databasePath, ws.WorkspaceID)
	if err != nil {
		return receiveFetch{}, fmt.Errorf("open sync store on the receive's clone: %w", err)
	}
	defer func() { err = errors.Join(err, closeClone()) }()
	target, err := resolveSyncTarget(ctx, session, ws, "")
	if err != nil {
		return receiveFetch{}, err
	}
	fetch.target = target
	if target.skip != syncTargetReady {
		return fetch, nil
	}
	fetch.fetchErr = session.syncer.SyncFetch(ctx, target.remote, false)
	return fetch, nil
}

// landFetch carries the clone's fetch to the live store (store.LandFetchedHead)
// and reports what it did to the tracking ref and how long it held the store,
// for the receive's trace. A mirror whose git objects did not land is reported
// and not failed: the chunks and the ref landed, which is the receive; what is
// lost is that the live store's next network operation downloads those
// objects again.
func landFetch(ctx context.Context, ws workspace.Info, clone liveClone, target syncTarget) (store.LandedFetch, error) {
	landed, err := store.LandFetchedHead(ctx, ws.DatabasePath, clone.databasePath, target.remote, target.branch)
	if errors.Is(err, store.ErrRemoteCacheNotLanded) {
		fmt.Fprintf(os.Stderr, "lit: automatic receive: %v\n", err)
		return landed, nil
	}
	return landed, err
}

// receiveCloneCutExplanation is the one wording of "the receive's clone take
// cut itself loose".
func receiveCloneCutExplanation() error {
	return fmt.Errorf("automatic receive's hold exceeded its %s budget cloning the store — a deadline, not a diagnosis; the live store is released as the cut unwinds, and the next receive retries", store.MirrorHoldBudget)
}
