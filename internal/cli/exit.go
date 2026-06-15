package cli

import (
	"errors"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

const (
	ExitOK         = 0
	ExitGeneric    = 1
	ExitUsage      = 2
	ExitValidation = 3
	ExitNotFound   = 4
	ExitConflict   = 5
	ExitCorruption = 7
)

// ExitCode maps a typed error to its exit code.
// [LAW:single-enforcer] This is the one place that decides exit code from error type.
// [LAW:types-are-the-program] Dispatch is by type (errors.As), never by message text.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var notFound store.NotFoundError
	if errors.As(err, &notFound) {
		return ExitNotFound
	}
	var mergeConflict MergeConflictError
	if errors.As(err, &mergeConflict) {
		return ExitConflict
	}
	var corruption CorruptionError
	if errors.As(err, &corruption) {
		return ExitCorruption
	}
	var usage UsageError
	if errors.As(err, &usage) {
		return ExitUsage
	}
	var unknownCmd UnknownCommandError
	if errors.As(err, &unknownCmd) {
		return ExitValidation
	}
	var validation ValidationError
	if errors.As(err, &validation) {
		return ExitValidation
	}
	var storeValidation store.ValidationError
	if errors.As(err, &storeValidation) {
		return ExitValidation
	}
	var unsupported UnsupportedError
	if errors.As(err, &unsupported) {
		return ExitValidation
	}
	var outsideWorkspace OutsideWorkspaceError
	if errors.As(err, &outsideWorkspace) {
		return ExitGeneric
	}
	if errors.Is(err, store.ErrTransientGCContention) {
		return ExitGeneric
	}
	return ExitGeneric
}
