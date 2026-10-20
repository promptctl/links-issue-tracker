package store

import (
	"context"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// SyncPush is SyncCompactAndPush without the compaction: a push, and on a
// refusal the fetch, follow and one retry every push runs
// (retryPushAfterFollow). It lives with the tests that push with it, never in
// the shipped package: storage.Syncer's pushes, SyncCompactAndPush and
// SyncPushFromClone, are each followed by SyncKeepHistoryBound, and a
// production push the bound never follows is what this keeps unwritable.
func (s *Store) SyncPush(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool, force bool) (storage.SyncPushResult, error) {
	result, pushErr := s.pushMutation(ctx, remote, branch, setUpstream, force)
	result, pushErr, _, followErr := s.retryPushAfterFollow(ctx, remote, branch, setUpstream, result, pushErr)
	if err := joinFollowFailure(pushErr, followErr); err != nil {
		return storage.SyncPushResult{}, err
	}
	return result, nil
}
