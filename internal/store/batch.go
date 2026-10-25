package store

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// batchResult is the account a batch builds of what it wrote; Written says it
// in words, for the refusal whose reset failed to name.
type batchResult interface{ Written() string }

// withBatch runs fn as one batch: everything fn writes lands, or none of it
// does. [LAW:single-enforcer] the one owner of a batch's atomicity in this
// engine, and of what a refused batch returns; an import file is written
// through it so a file refused at any write writes nothing, whichever rule
// refused it.
//
// The whole batch runs under ONE commit-lock hold. The lock is re-entrant, so
// every write fn makes — each its own Dolt commit — short-circuits its own
// acquisition, and no other writer, nor the mirror's clone (taken under the
// same lock), can land between them or see them half-made.
// [LAW:no-ambient-temporal-coupling]
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
// *RefusedBatchError when the reset landed; all of it beside a
// *BatchResetFailedError that names it when the reset failed. The observers
// hear the batch's changes (notify queues them) exactly when they stand, on
// the same two outcomes. [LAW:no-silent-failure] [LAW:nothing-unseen]
func withBatch[R batchResult](ctx context.Context, s *Store, fn func(ctx context.Context, result *R) error) (R, error) {
	var built, stands R
	err := s.withCommitLock(ctx, func(ctx context.Context) error {
		if err := s.commitStrandedWrites(ctx); err != nil {
			return fmt.Errorf("batch: %w", err)
		}
		head, err := readDoltHead(ctx, s.db)
		if err != nil {
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
			return &RefusedBatchError{Head: head, Cause: cause}
		}
		s.deliver(heard)
		stands = built
		return &BatchResetFailedError{Head: head, Cause: cause, ResetErr: resetErr, Written: built.Written()}
	})
	return stands, err
}

// RefusedBatchError is a batch refused partway and reset to the commit it
// started from, so nothing it wrote stands. Its text is the refusal's, and
// Unwrap reaches the refusal, so a caller classifies it exactly as it would
// the refusal alone.
// [LAW:types-are-the-program] A reset that failed is BatchResetFailedError:
// what the reset left is the type, not a flag on it.
type RefusedBatchError struct {
	// Head is the commit the batch started from, which the reset returned the
	// branch to.
	Head string
	// Cause is why the batch was refused.
	Cause error
}

func (e *RefusedBatchError) Error() string { return e.Cause.Error() }

func (e *RefusedBatchError) Unwrap() error { return e.Cause }

// EventRecord is the commit the refusal reset to, with an empty reset error.
func (e *RefusedBatchError) EventRecord() (map[string]any, map[string]int) {
	return batchResetFacts(e.Head, ""), nil
}

// BatchResetFailedError is a batch refused partway whose reset to the commit
// it started from failed too, so the writes made before the refusal are still
// committed. Written names them.
type BatchResetFailedError struct {
	// Head is the commit the batch started from, which the reset could not
	// return the branch to.
	Head string
	// Cause is why the batch was refused.
	Cause error
	// ResetErr is why the reset failed.
	ResetErr error
	// Written is the account of every write that stays, as the result the
	// import returns beside this error says it.
	Written string
}

func (e *BatchResetFailedError) Error() string {
	return fmt.Sprintf("%v\n\nresetting to commit %s, the head before the file, failed too: %v\nthe documents written before the refusal are still committed: %s",
		e.Cause, e.Head, e.ResetErr, e.Written)
}

func (e *BatchResetFailedError) Unwrap() []error { return []error{e.Cause, e.ResetErr} }

// EventRecord is the commit the reset could not reach, and why.
func (e *BatchResetFailedError) EventRecord() (map[string]any, map[string]int) {
	return batchResetFacts(e.Head, e.ResetErr.Error()), nil
}

// batchResetFacts is what a refused batch's reset left, under the keys both
// reset outcomes record: the commit it reset to, and why the reset failed,
// empty when it landed. [LAW:one-source-of-truth]
func batchResetFacts(head, resetErr string) map[string]any {
	return map[string]any{"import.reset_to": head, "import.reset_error": resetErr}
}

var (
	_ model.RecordedError = (*RefusedBatchError)(nil)
	_ model.RecordedError = (*BatchResetFailedError)(nil)
)
