package storage

import (
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// RefusedBatchError is a batch refused partway. A batch lands whole or not at
// all, so nothing a refused one wrote stands. It lives in the contract so a
// caller classifies a batch's end without reaching into an engine. Its text is
// the refusal's, and Unwrap reaches the refusal, so a caller classifies it
// exactly as it would the refusal alone. [LAW:types-are-the-program]
type RefusedBatchError struct {
	// Head is the commit the batch started from, which the branch still
	// holds; empty for an engine without commits.
	Head string
	// Cause is why the batch was refused.
	Cause error
}

func (e *RefusedBatchError) Error() string { return e.Cause.Error() }

func (e *RefusedBatchError) Unwrap() error { return e.Cause }

// EventRecord is the commit the refused file started from, so a refusal at a
// write reads apart from one the gate made before writing anything.
func (e *RefusedBatchError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{"import.head": e.Head}, nil
}

var _ model.RecordedError = (*RefusedBatchError)(nil)
