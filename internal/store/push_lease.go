package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/dolthub/dolt/go/store/datas"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// pushLeasedWithinLock pushes the local branch source to remote's branch,
// landing only while that branch still names expected, and otherwise leaving
// it where it is: DOLT_PUSH --force-with-lease, git's way to publish a
// rewritten history without overwriting a push that landed after the pusher
// last read the remote. A stale lease is an outcome,
// not an error — it is the one answer a lease exists to give — so the error
// is reserved for a push that reached no verdict. Where it stopped is the
// outcome beside it: none for a push refused before it reached the remote,
// LeaseUnsettled for one whose transport ran, since an attempt before the
// last may have landed. The caller holds the commit lock (via
// runSyncMutation), as it does for pushWithinLock. setUpstream is `lit sync
// push --set-upstream`, passed through as DOLT_PUSH's flag; the history
// bound's re-root never sets one.
//
// Beside the outcome is the push as the engine reported it, with Head the
// source's commit read under the lock, as pushWithinLock reports a plain push:
// the forced push is this lease (pushWithinLock), and its callers read the
// result a plain push gives. The result is zero for a push that reached no
// verdict.
//
// A transport retry that finds its own earlier attempt landed reports it
// held: a remote branch already at the pushed commit satisfies the lease
// (Patch 9 in FORKS.md). The verdict is the last attempt's, so a peer that
// built on a landed attempt before the retry reached the remote makes it
// stale (see storage.LeaseStale).
func (s *Store) pushLeasedWithinLock(ctx context.Context, remote storage.RemoteName, source string, branch string, expected string, setUpstream bool) (storage.LeaseOutcome, storage.SyncPushResult, error) {
	none := storage.SyncPushResult{}
	trimmedSource, err := requireSyncArg("source", source)
	if err != nil {
		return "", none, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return "", none, err
	}
	trimmedExpected := strings.TrimSpace(expected)
	if !isDoltCommitHash(trimmedExpected) {
		return "", none, fmt.Errorf("leased push to %s/%s: %q is not a Dolt commit hash", remote, trimmedBranch, expected)
	}
	// [LAW:single-enforcer] the same schema refusals every push runs, with R
	// read from the one commit a held lease can replace: expected. The
	// tracking ref may lag it, and a lease over a schema-ahead head would
	// regress the shared remote to this binary's schema.
	remoteHead, err := s.remoteHeadAt(ctx, trimmedExpected, remote, trimmedBranch)
	if err != nil {
		return "", none, err
	}
	if err := refuseSchemaAhead(remote, trimmedBranch, remoteHead); err != nil {
		return "", none, err
	}
	sourceHead, err := commitHashOfRef(ctx, s.db, trimmedSource)
	if err != nil {
		return "", none, err
	}
	if err := s.refuseSchemaSend(ctx, remote, trimmedBranch, remoteHead, sourceHead); err != nil {
		return "", none, err
	}
	args := []string{"--force-with-lease=" + trimmedBranch + ":" + trimmedExpected}
	if setUpstream {
		args = append(args, "--set-upstream")
	}
	args = append(args, remote.String(), trimmedSource+":"+trimmedBranch)
	query := buildProcedureCall("DOLT_PUSH", len(args))
	result := storage.SyncPushResult{Head: sourceHead}
	var message sql.NullString
	pushErr := s.runRemoteIO(ctx, func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx, query, stringArgsToAny(args)...).Scan(&result.Status, &message)
	})
	result.Message = nullStringValue(message)
	// [LAW:parse-dont-validate] the stale verdict is classified once, here,
	// and travels on as the outcome. DOLT_PUSH wraps datas.ErrStaleInfo for a
	// stale target, and the driver's error unwraps to it.
	switch {
	case errors.Is(pushErr, datas.ErrStaleInfo):
		return storage.LeaseStale, none, nil
	case pushErr != nil:
		return storage.LeaseUnsettled, none, classifyPushFailure(remote, pushErr)
	default:
		return storage.LeaseHeld, result, nil
	}
}
