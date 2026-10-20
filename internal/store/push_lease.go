package store

import (
	"context"
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
// is reserved for a push that reached no verdict. The caller holds the commit
// lock (via runSyncMutation), as it does for pushWithinLock.
//
// A transport retry that finds its own earlier attempt landed reports it
// held: a remote branch already at the pushed commit satisfies the lease
// (Patch 9 in FORKS.md). The verdict is the last attempt's, so a peer that
// built on a landed attempt before the retry reached the remote makes it
// stale (see storage.LeaseStale).
func (s *Store) pushLeasedWithinLock(ctx context.Context, remote, source, branch, expected string) (storage.LeaseOutcome, error) {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return "", err
	}
	trimmedSource, err := requireSyncArg("source", source)
	if err != nil {
		return "", err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return "", err
	}
	trimmedExpected := strings.TrimSpace(expected)
	if !isDoltCommitHash(trimmedExpected) {
		return "", fmt.Errorf("leased push to %s/%s: %q is not a Dolt commit hash", trimmedRemote, trimmedBranch, expected)
	}
	// [LAW:single-enforcer] the same schema-ahead refusal every push runs,
	// on the one commit a held lease can replace: expected. The tracking ref
	// may lag it, and a lease over a schema-ahead head would regress the
	// shared remote to this binary's schema.
	if err := s.guardCommitSchemaAhead(ctx, trimmedRemote, trimmedBranch, trimmedExpected); err != nil {
		return "", err
	}
	args := []string{"--force-with-lease=" + trimmedBranch + ":" + trimmedExpected, trimmedRemote, trimmedSource + ":" + trimmedBranch}
	query := buildProcedureCall("DOLT_PUSH", len(args))
	pushErr := runRemoteIO(ctx, func(ctx context.Context) error {
		var status int64
		var message *string
		return s.db.QueryRowContext(ctx, query, stringArgsToAny(args)...).Scan(&status, &message)
	})
	// [LAW:parse-dont-validate] the stale verdict is classified once, here,
	// and travels on as the outcome. The SQL boundary renders DOLT_PUSH's
	// error as text (MySQL error 1105), so it is recognized by the text of
	// Dolt's own sentinel, which DOLT_PUSH carries for a stale target.
	switch {
	case pushErr != nil && strings.Contains(pushErr.Error(), datas.ErrStaleInfo.Error()):
		return storage.LeaseStale, nil
	case pushErr != nil:
		return "", classifyPushFailure(ctx, trimmedRemote, pushErr)
	default:
		return storage.LeaseHeld, nil
	}
}
