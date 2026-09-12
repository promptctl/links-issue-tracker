package cli

import (
	"errors"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

const (
	ExitOK         = 0
	ExitGeneric    = 1
	ExitUsage      = 2
	ExitValidation = 3
	ExitNotFound   = 4
	ExitConflict   = 5
	// ExitNoWork: the command ran correctly and changed nothing — it had no
	// ticket to hand back, or the state it was asked for already held.
	// Distinct from ExitGeneric because a caller looping `lit next` has to tell
	// "stop, there is nothing for you" from "lit is broken", and under one code
	// its only way to do that was to parse the English — the thing every other
	// sink in this package exists to stop callers doing. The already-in-state
	// half is the same question asked of a mutation ("did I need to do
	// anything?") and gets the same answer rather than a code of its own.
	//
	// Not ExitOK: for `lit next`, 0 means "a ticket is on stdout". Exiting 0
	// with no row would hand the caller a success-shaped void.
	// [LAW:parse-dont-validate]
	ExitNoWork     = 6
	ExitCorruption = 7
)

// ExitCode maps a typed error to its exit code.
// [LAW:single-enforcer] This is the one place that decides exit code from error type.
// [LAW:types-are-the-program] Dispatch is by type (errors.As), never by message text.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var notFound storage.NotFoundError
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
	// A template whose shape cannot converge is a deterministic refusal of the
	// data the command was given, which is what ExitValidation means — the act
	// it asks for differs (edit the file, not the flags), and that difference is
	// carried by the reason, not by a code of its own. [LAW:one-source-of-truth]
	var templateShape templateShapeError
	if errors.As(err, &templateShape) {
		return ExitValidation
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
	var storeValidation storage.ValidationError
	if errors.As(err, &storeValidation) {
		return ExitValidation
	}
	// The two halves of a container refusal are different answers and exit
	// differently. A request the children already satisfy ran correctly and
	// changed nothing — the same shape as the router's "nothing to hand back",
	// so it shares that code rather than inventing a second one for it. A
	// refusal is a domain-constraint rejection like any other. Neither is
	// ExitGeneric, which is what made the release-closing `lit done <epic>`
	// report its workflow's final step as a failure while the state it asked
	// for was exactly the state that held. [LAW:no-mode-explosion]
	var containerAction model.ContainerActionError
	if errors.As(err, &containerAction) {
		if containerAction.Satisfied() {
			return ExitNoWork
		}
		return ExitValidation
	}
	var unsupported UnsupportedError
	if errors.As(err, &unsupported) {
		return ExitValidation
	}
	// Both of the router's terminal answers share one code: the caller's
	// question here is binary — was a ticket handed back? — and which of the two
	// emptinesses it was is carried by the reason string and the message.
	// [LAW:no-mode-explosion]
	var exhausted Exhausted
	if errors.As(err, &exhausted) {
		return ExitNoWork
	}
	var noWork NoWork
	if errors.As(err, &noWork) {
		return ExitNoWork
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
