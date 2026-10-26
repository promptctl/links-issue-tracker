package store

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// batchResult is the account a batch builds of what it wrote; Written says it
// in words, for the refusal whose reset failed to name.
type batchResult interface{ Written() string }

// withBatch runs fn as one batch: a refusal at any write fn makes leaves the
// branch where the batch found it. [LAW:single-enforcer] the one owner of what
// a refused batch leaves in this engine, and of what it returns; an import
// file is written through it so a file refused at any write writes nothing,
// whichever rule refused it.
//
// The whole batch runs under ONE commit-lock hold. The lock is re-entrant, so
// every write fn makes — each its own Dolt commit — short-circuits its own
// acquisition, and no other writer, nor the mirror's clone (taken under the
// same lock), can land between them or see them half-made.
// [LAW:no-ambient-temporal-coupling] A read engine takes no lock, so a read
// during the batch can see writes a refusal then resets away; and the reset
// runs only on fn's error, so a process killed partway leaves what it wrote.
//
// The revert point is the head commit the batch starts from, which stays
// reachable as an ancestor of the branch for as long as the batch runs, so
// there is nothing to create or clean up. A write a previous process staged
// and never committed is committed first (commitStrandedWrites), so the reset
// cannot discard work that is not the batch's. On fn's error the branch is
// hard-reset to the revert point on a context the caller's cancellation cannot
// reach, so a Ctrl-C that refused the batch cannot also stop its reset.
//
// fn builds the batch's result in place, and withBatch returns what of it
// stands: all of it when the batch lands; the zero result beside a
// *storage.RefusedBatchError when the reset landed; all of it beside a
// *storage.BatchResetFailedError that names it when the reset failed. The observers
// hear the batch's changes (notify queues them) exactly when they stand, on
// the same two outcomes. [LAW:no-silent-failure] [LAW:nothing-unseen]
func withBatch[R batchResult](ctx context.Context, s *Store, fn func(ctx context.Context, result *R) error) (R, error) {
	var built, stands R
	err := s.withCommitLock(ctx, func(ctx context.Context) error {
		// Retried as every write is, since nothing is written yet: a re-run
		// finds the stranded writes already committed and reads the same head.
		var head string
		if err := retryTransientGCContention(ctx, func(ctx context.Context) error {
			if err := s.commitStrandedWrites(ctx); err != nil {
				return err
			}
			var err error
			head, err = readDoltHead(ctx, s.db)
			return err
		}, s.reconnect); err != nil {
			return fmt.Errorf("batch: %w", err)
		}
		var heard []heardChange
		cause := fn(context.WithValue(ctx, batchHearingKey{}, &heard), &built)
		if cause == nil {
			s.deliver(heard)
			stands = built
			return nil
		}
		resetErr := resetHardToRef(context.WithoutCancel(ctx), s.db, head)
		if resetErr == nil {
			return &storage.RefusedBatchError{Head: head, Cause: cause}
		}
		s.deliver(heard)
		stands = built
		return &storage.BatchResetFailedError{Head: head, Cause: cause, ResetErr: resetErr, Written: built.Written()}
	})
	return stands, err
}
