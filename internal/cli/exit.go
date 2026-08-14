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
	// A non-transient sync divergence the agent must resolve is a conflict-class
	// exit — the same code an unresolved reconcile already uses, so the held-conflict
	// exit contract is one value across `lit sync pull` and `lit sync reconcile`.
	// [LAW:one-source-of-truth]
	var syncFailure SyncFailureError
	if errors.As(err, &syncFailure) {
		return ExitConflict
	}
	// The take gate's refusal is the same unresolved-divergence condition
	// persisting — the take did not run — so it shares the conflict exit.
	// [LAW:one-source-of-truth]
	var ownerApproval ownerApprovalRefusalError
	if errors.As(err, &ownerApproval) {
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
	// A retired command sits alongside an unknown one: the name is no longer a
	// working command, so the caller must pick a different path. Same class.
	var retiredCmd RetiredCommandError
	if errors.As(err, &retiredCmd) {
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
	var bulkFailure BulkFailureError
	if errors.As(err, &bulkFailure) {
		// Per-item failures are heterogeneous (not-found, conflict, …); the exit
		// code carries only the binary any-failed signal, while the per-item
		// typed reasons live in the stderr message. [LAW:no-silent-failure]
		return ExitGeneric
	}
	if errors.Is(err, store.ErrTransientGCContention) {
		return ExitGeneric
	}
	return ExitGeneric
}
