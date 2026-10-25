package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// withBatch runs fn as one batch: everything fn writes lands, or none of it
// does. [LAW:single-enforcer] the one owner of a batch's atomicity in this
// engine; an import file is written through it so a file refused at any write
// writes nothing, whichever rule refused it.
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
// reach, so a Ctrl-C that refused the batch cannot also stop its reset. Every
// refusal is a *RefusedBatchError naming the commit it reset to, and whether
// the reset landed. The observers hear the batch's changes (notify queues them)
// exactly when they stand: when the batch lands, or when its reset failed.
// [LAW:no-silent-failure] [LAW:nothing-unseen]
func (s *Store) withBatch(ctx context.Context, fn func(ctx context.Context) error) error {
	return s.withCommitLock(ctx, func(ctx context.Context) error {
		if err := s.commitStrandedWrites(ctx); err != nil {
			return fmt.Errorf("batch: %w", err)
		}
		head, err := readDoltHead(ctx, s.db)
		if err != nil {
			return fmt.Errorf("batch: %w", err)
		}
		var heard []heardChange
		cause := fn(context.WithValue(ctx, batchHearingKey{}, &heard))
		if cause == nil {
			s.deliver(heard)
			return nil
		}
		resetErr := resetHardToRef(context.WithoutCancel(ctx), s.db, head)
		if resetErr != nil {
			// The writes stand, so their observers are owed them.
			s.deliver(heard)
		}
		return &RefusedBatchError{Head: head, Cause: cause, ResetErr: resetErr}
	})
}

// RefusedBatchError is a batch refused partway and reset to the commit it
// started from. Its text is the refusal's, and Unwrap reaches the refusal, so a
// caller classifies it exactly as it would the refusal alone. When the reset
// failed too, ResetErr says why, the writes made before the refusal are still
// committed, and Written names them.
type RefusedBatchError struct {
	// Head is the commit the batch started from, which the reset returned the
	// branch to, or could not.
	Head string
	// Cause is why the batch was refused.
	Cause error
	// ResetErr is why the reset failed, nil when it landed.
	ResetErr error
	// Written is the account of every write that stays when the reset failed,
	// rendered by the result the import returns beside this error.
	Written string
}

func (e *RefusedBatchError) Error() string {
	if e.ResetErr == nil {
		return e.Cause.Error()
	}
	return fmt.Sprintf("%v\n\nresetting to commit %s, the head before the file, failed too: %v\nthe documents written before the refusal are still committed: %s",
		e.Cause, e.Head, e.ResetErr, e.Written)
}

func (e *RefusedBatchError) Unwrap() []error { return []error{e.Cause, e.ResetErr} }

// EventRecord is the commit the refusal reset to and, when the reset failed,
// why: import.reset_error is empty when the reset landed.
func (e *RefusedBatchError) EventRecord() (map[string]any, map[string]int) {
	resetErr := ""
	if e.ResetErr != nil {
		resetErr = e.ResetErr.Error()
	}
	return map[string]any{"import.reset_to": e.Head, "import.reset_error": resetErr}, nil
}

var _ model.RecordedError = (*RefusedBatchError)(nil)

// refusedBatch is what an import returns for a batch withBatch refused: the
// zero result when the reset left nothing written, and, when the reset failed
// too, what the batch wrote beside an error that says it in words.
func refusedBatch[R interface{ Written() string }](written R, err error) (R, error) {
	var refused *RefusedBatchError
	if !errors.As(err, &refused) || refused.ResetErr == nil {
		var zero R
		return zero, err
	}
	refused.Written = written.Written()
	return written, refused
}
