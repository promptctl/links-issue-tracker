package storage

import (
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The two ways a refused batch ends, as the BulkWriter contract names them:
// put back, or not. They live in the contract so a caller classifies a batch's
// end without reaching into an engine. [LAW:types-are-the-program]

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
