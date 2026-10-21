package store

import (
	"context"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// SyncPush is SyncCompactAndPush without the compaction or the history bound:
// a push, and on a refusal the fetch, follow and one retry every push runs
// (retryPushAfterFollow). It lives with the tests that push with it, never in
// the shipped package: storage.Syncer's pushes, SyncCompactAndPush and
// SyncPushFromClone, each keep the bound of a push they land, and a
// production push without it is what this keeps unwritable.
func (s *Store) SyncPush(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool, force bool) (storage.SyncPushResult, error) {
	result, pushErr := s.pushMutation(ctx, remote, branch, setUpstream, force)
	result, pushErr, followErr := s.retryPushAfterFollow(ctx, remote, branch, setUpstream, result, pushErr)
	s.collectOwedCut(ctx, storage.SyncEntryPush)
	if err := joinFollowFailure(pushErr, followErr); err != nil {
		return storage.SyncPushResult{}, err
	}
	return result, nil
}
