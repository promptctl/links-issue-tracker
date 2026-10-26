package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// WriteCommandError renders a failed command to stderr: a header carrying the
// exit code and the error's typed reason, the message, then the actionable
// remediation for that reason. Text is the one canonical surface, so the
// remediation guidance reaches every caller. [LAW:single-enforcer] The
// error→reason→remediation mapping is derived in one boundary.
//
// The reason rides in the header beside the code because the exit code is
// deliberately coarse — exit 3 alone covers a value the rules refuse, a
// directory that is not a repository, and a repository with no workspace, each
// cleared by a different act — and the header is the one line whose position is fixed: a
// message can span lines, so a reason printed after it would sit at an offset
// no caller can find without parsing the message.
func WriteCommandError(stderr io.Writer, err error) int {
	reason := commandErrorReason(err)
	_, _ = fmt.Fprintf(stderr, "error (code=%d, reason=%s): %v\n", reason.exit, reason, err)
	if remediation := commandErrorRemediation(reason); remediation != "" {
		_, _ = fmt.Fprintf(stderr, "remediation: %s\n", remediation)
	}
	return reason.exit
}

// commandReason is the machine-readable reason a failed command prints beside
// its exit code, and the exit code it arrives on. The tokens are a published
// contract (docs/cli-reference.md lists each with its code): a script branches
// on them where one exit code covers failures that call for different acts.
// [LAW:one-source-of-truth] a reason is declared once, with its code, so the
// code is a property of the reason and no second classifier can give a failure
// a code its reason does not carry. [LAW:types-are-the-program] each token is
// spelled once, here, so the classifier and the remediation switch cannot
// disagree on a spelling and silently fall through to the default retry advice.
type commandReason struct {
	token string
	exit  int
}

func (r commandReason) String() string { return r.token }

// Every reason is an unkeyed literal, which does not compile without its code.
var (
	// ExitGeneric: lit could not do what was asked. A bulk run whose every item
	// answers with one reason answers with that reason (bulkReason); this one
	// is a run whose items differ — some landed, or the rest did not land
	// alike — so no single code is true of them all and the per-item reasons
	// live in the message. [LAW:no-silent-failure]
	reasonBulkPartialFailure    = commandReason{"bulk_partial_failure", ExitGeneric}
	reasonCommandFailed         = commandReason{"command_failed", ExitGeneric}
	reasonMigrationFailed       = commandReason{"migration_failed", ExitGeneric}
	reasonRemoteUnreachable     = commandReason{"remote_unreachable", ExitGeneric}
	reasonTransientGcContention = commandReason{"transient_gc_contention", ExitGeneric}
	reasonWorkspaceBusy         = commandReason{"workspace_busy", ExitGeneric}
	reasonWorkspaceWriteBlocked = commandReason{"workspace_write_blocked", ExitGeneric}
	// An import refused partway whose reset failed too: the writes ahead of
	// the refusal are still committed, so neither the refusal's reason nor a
	// retry is true of the store.
	reasonImportResetFailed = commandReason{"import_reset_failed", ExitGeneric}

	reasonUsageError = commandReason{"usage_error", ExitUsage}

	// ExitValidation: deterministic for the command as issued, so no retry
	// clears it. That covers a refusal of the command, of a file or stored
	// value it reads, and of the environment it ran in: no repository, no
	// workspace, a schema past this binary, a quarantined migration. None is
	// ExitGeneric, which also means "lit is broken": under one code a script
	// could tell a self-fixable precondition from a fault only by parsing the
	// English. The act each calls for differs (adjust the command, edit a
	// file, run `lit init`, install a different lit), and the reason printed
	// beside the code carries that difference, not a code of its own.
	// [LAW:no-mode-explosion]
	reasonMigrationPartiallyApplied = commandReason{"migration_partially_applied", ExitValidation}
	reasonMigrationQuarantined      = commandReason{"migration_quarantined", ExitValidation}
	reasonSchemaShapeRefused        = commandReason{"schema_shape_refused", ExitValidation}
	reasonOutsideGitWorkspace       = commandReason{"outside_git_workspace", ExitValidation}
	reasonPurgeUnconfirmed          = commandReason{"purge_unconfirmed", ExitValidation}
	reasonRetiredCommand            = commandReason{"retired_command", ExitValidation}
	reasonStoredPrefixRefused       = commandReason{"stored_prefix_refused", ExitValidation}
	reasonTakeoverUnconfirmed       = commandReason{"takeover_unconfirmed", ExitValidation}
	reasonTemplateShapeRefused      = commandReason{"template_shape_refused", ExitValidation}
	reasonUnknownCommand            = commandReason{"unknown_command", ExitValidation}
	reasonUnsupportedFlag           = commandReason{"unsupported_flag", ExitValidation}
	reasonValidationRefused         = commandReason{"validation_refused", ExitValidation}
	reasonWorkspaceNotInitialized   = commandReason{"workspace_not_initialized", ExitValidation}
	reasonWorkspaceSchemaAhead      = commandReason{"workspace_schema_ahead", ExitValidation}

	reasonEntityNotFound  = commandReason{"entity_not_found", ExitNotFound}
	reasonVersionNotFound = commandReason{"version_not_found", ExitNotFound}

	// ExitConflict: a divergence the agent must resolve. A non-transient sync
	// divergence shares the code an unresolved reconcile uses, so the
	// held-conflict exit is one value across `lit sync pull` and `lit sync
	// reconcile`, and the take gate's refusal is that same divergence
	// persisting. [LAW:one-source-of-truth]
	reasonMergeConflict         = commandReason{"merge_conflict", ExitConflict}
	reasonOwnerApprovalRequired = commandReason{"owner_approval_required", ExitConflict}
	reasonRestoreWouldOverwrite = commandReason{"restore_would_overwrite", ExitConflict}
	reasonSyncDivergence        = commandReason{"sync_divergence", ExitConflict}
	// A change a later write stands in the way of: an undo putting back a slot
	// moved again, or a read-out file whose issues moved since the read.
	// Landing it would erase that write, so the agent decides what goes.
	reasonWriteConflict = commandReason{"write_conflict", ExitConflict}

	// ExitNoWork: the command ran correctly and changed nothing. The router's
	// three terminal answers share it because the caller's question is binary
	// (was a ticket handed back?), and a state request that already holds is
	// the same answer asked of a mutation; under ExitGeneric the
	// release-closing `lit done <epic>` would report its workflow's final step
	// as a failure while the state it asked for is exactly the state that
	// holds. Which emptiness it was is the reason's to say.
	// [LAW:no-mode-explosion]
	reasonNoReadyWork       = commandReason{"no_ready_work", ExitNoWork}
	reasonScopeExhausted    = commandReason{"scope_exhausted", ExitNoWork}
	reasonScopeNarrowed     = commandReason{"scope_narrowed", ExitNoWork}
	reasonStateAlreadyHolds = commandReason{"state_already_holds", ExitNoWork}

	reasonCorruptionDetected = commandReason{"corruption_detected", ExitCorruption}
	reasonParentCycle        = commandReason{"parent_cycle", ExitCorruption}
)

// commandErrorReason maps a typed error to its reason, and so to its exit code.
// [LAW:single-enforcer] This is the one classifier: every error→reason mapping
// lives here, dispatched by type via errors.As, and ExitCode reads the code off
// its answer.
// [LAW:types-are-the-program] No message text is inspected; classification is carried by the type.
func commandErrorReason(err error) commandReason {
	// A failure of an open's schema convergence: the act that clears it is a
	// property of the step that failed, read off the step. A step that may
	// have left partial changes, or a shape no open converges, outranks
	// everything else in the tree: a contended reset still leaves a dirty
	// working set, and the busy and GC-contention arms below would answer
	// "retry" to it. A step a retry clears is classified like any other error,
	// so a wrapper's reason or the cause's own reason still holds, and only an
	// error no arm names gets migration_failed rather than command_failed,
	// whose doctor advice cannot help: doctor opens the store through the
	// same convergence. [LAW:single-enforcer] [LAW:one-type-per-behavior]
	unnamed := reasonCommandFailed
	var convergence *store.SchemaConvergenceError
	if errors.As(err, &convergence) {
		switch convergence.Step.Act() {
		case store.ActRestoreSnapshot:
			return reasonMigrationPartiallyApplied
		case store.ActEscalate:
			return reasonSchemaShapeRefused
		case store.ActClearCause:
			unnamed = reasonMigrationFailed
		}
	}
	return typedErrorReason(err, unnamed)
}

// typedErrorReason is the reason err's types name, or unnamed when none does.
func typedErrorReason(err error, unnamed commandReason) commandReason {
	// Ahead of every arm its cause could reach: a refusal's reason would say
	// "change the input and re-run", and a re-run creates the file's creates a
	// second time over the ones the failed reset left. [LAW:types-are-the-program]
	var resetFailed *storage.BatchResetFailedError
	if errors.As(err, &resetFailed) {
		return reasonImportResetFailed
	}
	var notFound storage.NotFoundError
	if errors.As(err, &notFound) {
		return reasonEntityNotFound
	}
	var versionNotFound storage.VersionNotFoundError
	if errors.As(err, &versionNotFound) {
		return reasonVersionNotFound
	}
	var planDigestNotFound storage.PlanDigestNotFoundError
	if errors.As(err, &planDigestNotFound) {
		return reasonVersionNotFound
	}
	var mergeConflict MergeConflictError
	if errors.As(err, &mergeConflict) {
		return reasonMergeConflict
	}
	var restoreOverwrite RestoreOverwriteError
	if errors.As(err, &restoreOverwrite) {
		return reasonRestoreWouldOverwrite
	}
	var writeConflict storage.ConflictError
	if errors.As(err, &writeConflict) {
		return reasonWriteConflict
	}
	// A schema ahead of this binary: a workspace it refuses to open or cannot
	// downgrade, or a remote it will not write below. Terminal for the binary
	// that raised it — no rerun changes which lit is installed — so it must not
	// reach the default's retry advice. Not validation_refused: the command is
	// not what is wrong, so "adjust the command" would be false advice. The
	// remote refusal arrives wrapped in the sync-failure block that renders it,
	// so this arm precedes the block's: the block is how it reads, not what it
	// is. [LAW:one-type-per-behavior]
	var schemaAhead model.SchemaAhead
	if errors.As(err, &schemaAhead) {
		return reasonWorkspaceSchemaAhead
	}
	var syncFailure SyncFailureError
	if errors.As(err, &syncFailure) {
		return reasonSyncDivergence
	}
	var remoteUnreachable store.RemoteUnreachableError
	if errors.As(err, &remoteUnreachable) {
		return reasonRemoteUnreachable
	}
	var ownerApproval ownerApprovalRefusalError
	if errors.As(err, &ownerApproval) {
		return reasonOwnerApprovalRequired
	}
	// A hierarchy that loops is corruption with one known repair, which `lit
	// doctor --fix integrity` is not, so it precedes the corruption arm: a
	// doctor report that found the loop carries it as its cause.
	// [LAW:one-type-per-behavior]
	var parentCycle storage.ParentCycleError
	if errors.As(err, &parentCycle) {
		return reasonParentCycle
	}
	var corruption CorruptionError
	if errors.As(err, &corruption) {
		return reasonCorruptionDetected
	}
	var unknownCmd UnknownCommandError
	if errors.As(err, &unknownCmd) {
		return reasonUnknownCommand
	}
	var retiredCmd RetiredCommandError
	if errors.As(err, &retiredCmd) {
		return reasonRetiredCommand
	}
	var usage UsageError
	if errors.As(err, &usage) {
		return reasonUsageError
	}
	// A domain-constraint refusal (exit 3), deterministic for the command as
	// issued. It must never fall to the default "Retry the command" — a
	// refusal's message already names the rule (and often the alternative), and
	// an agent that trusts remediation text over the error body will loop on a
	// retry that can never succeed.
	var refusal model.Refusal
	if errors.As(err, &refusal) {
		return reasonValidationRefused
	}
	// A malformed managed template is a refusal of a file on disk, not of the
	// command as issued, so it must not inherit validation_refused's "adjust the
	// command" — the command is already right and rerunning it unchanged is the
	// loop this whole mapping exists to prevent. [LAW:one-type-per-behavior]
	var templateShape templateShapeError
	if errors.As(err, &templateShape) {
		return reasonTemplateShapeRefused
	}
	// A command that asked for a state: an action on an epic, whose state is its
	// children's to set, or a retention move. Both halves are terminal — no
	// retry of the same command can move either — so neither may reach the
	// default's "Retry the command", which would tell the agent closing a
	// finished epic to loop on a condition that cannot change. They are
	// separate reasons because the act each calls for is different: one asks
	// for a state that already holds and wants nothing done at all, the other
	// asks for something the command as issued cannot do, and a single reason
	// could only name one of them. [LAW:one-type-per-behavior]
	var stateRequest model.StateRequestError
	if errors.As(err, &stateRequest) {
		if stateRequest.Satisfied() {
			return reasonStateAlreadyHolds
		}
		return reasonValidationRefused
	}
	// The router's three terminal answers. None is a fault: routing asked a
	// well-formed question and the honest reply was "nothing to hand you". They
	// are separate reasons because the act each calls for is different — one
	// asks you to move work you already hold, one to drop the filter that hid
	// your scope's next ticket, the other to widen the question or create work
	// — and a single reason could only name one of them.
	// [LAW:one-type-per-behavior]
	var exhausted Exhausted
	if errors.As(err, &exhausted) {
		return reasonScopeExhausted
	}
	var narrowed Narrowed
	if errors.As(err, &narrowed) {
		return reasonScopeNarrowed
	}
	var noWork NoWork
	if errors.As(err, &noWork) {
		return reasonNoReadyWork
	}
	// A retired flag is refused on every run, so it must never reach the
	// default "Retry the command". [LAW:no-silent-failure]
	var unsupported UnsupportedError
	if errors.As(err, &unsupported) {
		return reasonUnsupportedFlag
	}
	// A lane another checkout holds right now, not taken over. Not
	// validation_refused: that line says "adjust the command", and the one
	// adjustment that clears this is a flag the remediation has to name.
	var takeover takeoverUnconfirmedError
	if errors.As(err, &takeover) {
		return reasonTakeoverUnconfirmed
	}
	// A purge not confirmed. Its own reason for the same cause as takeover's:
	// the adjustment that clears it is a flag the remediation has to name.
	var purge purgeUnconfirmedError
	if errors.As(err, &purge) {
		return reasonPurgeUnconfirmed
	}
	var outsideWorkspace OutsideWorkspaceError
	if errors.As(err, &outsideWorkspace) {
		return reasonOutsideGitWorkspace
	}
	// The other negative answer to "am I somewhere lit can work?": a git
	// repository that `lit init` has never run in. Terminal in the same way —
	// no rerun of the same command can make the workspace exist — so it must
	// not reach the default's retry advice, which points the agent at a loop
	// and at `lit doctor`, a command that reads the very workspace that is
	// missing. It carries its own reason rather than sharing
	// outside_git_workspace's because the act differs: initialize here, versus
	// go somewhere already initialized. [LAW:one-type-per-behavior]
	if errors.Is(err, store.ErrWorkspaceNotInitialized) {
		return reasonWorkspaceNotInitialized
	}
	// A pending migration that failed and is quarantined: on the run that
	// failed, or any open after it. Every open that has it pending refuses
	// until the quarantine row is cleared or a snapshot from before the
	// failure is restored, so it must not reach the default's retry advice. Not
	// validation_refused: the command is not what is wrong, so "adjust the
	// command" would be false advice. [LAW:one-type-per-behavior]
	var quarantined *store.QuarantineBlockError
	if errors.As(err, &quarantined) {
		return reasonMigrationQuarantined
	}
	// lit could not settle on an issue prefix, and the three ways it can fail
	// split by the ACT that clears them, which is what a reason names.
	//
	// A stored issue_prefix the rules refuse is a refusal of the workspace, not
	// of the command as issued: the command is fine, and a different one —
	// `lit prefix set` — has to run first. It takes its own reason for the same
	// cause template_shape_refused has one: validation_refused's remediation
	// ends "adjust the command to satisfy it", which is false here, and an agent
	// that acts on the remediation line rather than on the message body is the
	// loop this whole mapping exists to prevent. Checked BEFORE the sentinel it
	// unwraps to, so the narrower answer wins.
	// [LAW:one-type-per-behavior]
	var storedPrefix workspace.StoredPrefixError
	if errors.As(err, &storedPrefix) {
		return reasonStoredPrefixRefused
	}
	// The other two — a repository name that yields no prefix, and an explicit
	// --prefix contradicting the one this workspace already carries — are both
	// answered by adjusting the command, which is what validation_refused means,
	// so they share it and each message names its own act.
	if errors.Is(err, workspace.ErrIssuePrefixRefused) {
		return reasonValidationRefused
	}
	var bulk BulkError
	if errors.As(err, &bulk) {
		return bulkReason(bulk)
	}
	// A write blocked by another process holding the store is checked BEFORE the
	// transient-contention catch-all: WorkspaceWriteBlockedError unwraps to the
	// transient cause (so errors.Is(err, ErrTransientGCContention) is still true),
	// but the terminal holder situation has its own reason and remediation. Order
	// makes errors.As win over the errors.Is fallthrough. [LAW:types-are-the-program]
	var writeBlocked store.WorkspaceWriteBlockedError
	if errors.As(err, &writeBlocked) {
		return reasonWorkspaceWriteBlocked
	}
	// A store lock and the config lock are two locks with one answer: another
	// lit process holds what this one needs, and a retry after it exits succeeds.
	if errors.Is(err, store.ErrWorkspaceBusy) || errors.Is(err, workspace.ErrConfigBusy) {
		return reasonWorkspaceBusy
	}
	if errors.Is(err, store.ErrTransientGCContention) {
		return reasonTransientGcContention
	}
	return unnamed
}

// bulkReason is a bulk run's reason: the one every item it ran answers with,
// so a run over one item answers as its single command does and a run refused
// alike answers as a refusal, or bulk_partial_failure when they differ. A landed
// item answers with no reason, so a run in which any item landed differs: under
// its unlanded items' shared reason, state_already_holds would claim the run
// changed nothing and a retry reason would re-apply what landed.
// [LAW:single-enforcer] each item is classified by commandErrorReason, the one
// classifier, not by a bulk copy.
func bulkReason(bulk BulkError) commandReason {
	counts := map[commandReason]int{}
	for _, item := range bulk.Unlanded {
		counts[commandErrorReason(item.Err)]++
	}
	for reason, n := range counts {
		if n == bulk.Ran {
			return reason
		}
	}
	return reasonBulkPartialFailure
}

// installReplacesLit is why installing a lit is the user's call: the
// workspace_schema_ahead remediation and the remote-schema-ahead sync block
// both gate `lit upgrade` on it. [LAW:one-source-of-truth]
const installReplacesLit = "Installing a different lit replaces it for every workspace on this machine"

func commandErrorRemediation(reason commandReason) string {
	switch reason {
	case reasonUnknownCommand:
		return "Run `lit --help` (or `lit help <command>`) to select a supported command path."
	case reasonRetiredCommand:
		// The error message already names the replacement command(s), so a second
		// remediation line would be a drifting copy of it. Emit none.
		// [LAW:one-source-of-truth]
		return ""
	case reasonUsageError:
		return "Run the command with `--help` and retry with valid arguments."
	case reasonUnsupportedFlag:
		return "Do not retry unchanged — this flag is refused on every run. Drop it and use what the message above names instead."
	case reasonEntityNotFound:
		return "Verify the target ID exists with `lit ls` or `lit show <id>`."
	case reasonVersionNotFound:
		// The message names how many versions there are and where the current
		// text is, so a remediation line would be a drifting copy of it. Emit
		// none. [LAW:one-source-of-truth]
		return ""
	case reasonMergeConflict:
		return "Sync and retry after resolving conflicts."
	case reasonWriteConflict:
		// The message names the way on for the change it refused (an undo's
		// or a read-out file's), so a remediation line here would be a
		// drifting copy of it. Emit none. [LAW:one-source-of-truth]
		return ""
	case reasonRestoreWouldOverwrite:
		return "Rerun with `--force` to restore anyway: the restore first takes a backup of the workspace it replaces, and `lit backup list` names it."
	case reasonSyncDivergence:
		// The SyncFailureError message IS the full remediation (directive + steps +
		// escalation), so a second remediation line here would be a drifting copy of
		// it. Emit none. [LAW:one-source-of-truth]
		return ""
	case reasonRemoteUnreachable:
		// Accurate for what actually happened: the network path failed and lit
		// already spent its retry budget. Neither credentials nor `lit doctor`
		// are involved — the workspace is healthy. [LAW:no-silent-failure] the
		// guidance points at the real fault domain, not a generic retry.
		return "The remote host was unreachable over the network; credentials are not the problem, and lit already retried with backoff. Check connectivity to the remote host (for SSH remotes: `ssh -o BatchMode=yes git@<host>`), then retry once the network path is restored."
	case reasonTemplateShapeRefused:
		return "Edit the template override the message names so it is either plain content with no LIT INTEGRATION markers or exactly one whole marked block, or delete the override to fall back to lit's bundled default. The command itself is fine; rerunning it unchanged repeats this refusal."
	case reasonStoredPrefixRefused:
		// Names the repair, and the preview before it: the right new prefix is
		// usually the one the store's existing ids already use, and the preview is
		// what shows it, so a typo is repaired to the original rather than
		// rewritten to whatever the caller guesses. [LAW:no-silent-failure] the
		// advice has to be an act that works, not merely present.
		return "Run `lit prefix set <prefix>` to preview the repair — the preview lists the prefixes this store's issue ids already use — then rerun it with `--apply`. The command itself is fine; rerunning it unchanged repeats this refusal."
	case reasonValidationRefused:
		return "Do not retry unchanged — this refusal is deterministic and will repeat until the command or the data changes. The error message above states the rule it enforces; adjust the command to satisfy it."
	case reasonWorkspaceBusy:
		// Each contention wrapper carries its own retry guidance, so a remediation
		// line would be a drifting copy of it. Emit none. [LAW:one-source-of-truth]
		return ""
	case reasonOwnerApprovalRequired:
		// Same shape: the refusal block IS the remediation. [LAW:one-source-of-truth]
		return ""
	case reasonParentCycle:
		// The message names the loop and the repair; this line says only that
		// nothing else will clear it. [LAW:one-source-of-truth]
		return "Do not retry unchanged: the loop stays until one of its parent edges is replaced. Break it the way the message above names, then rerun the command."
	case reasonCorruptionDetected:
		return "Run `lit doctor --fix integrity` and retry. " + agentInstructionsOpen + "This command is idempotent and safe to run without confirmation." + agentInstructionsClose
	case reasonTransientGcContention:
		// `lit doctor --fix` is not named: doctor opens the store through the
		// same convergence, and its fixes (integrity, rank) clear neither
		// contention nor a held store. [LAW:no-silent-failure]
		return "Retry once: online garbage collection held the store for a moment. If it fails the same way again, surface it to the user as blocking."
	case reasonMigrationFailed:
		// `lit doctor` is not named even to rule it out: doctor opens the store
		// through the same migration, and an agent that skims acts on the
		// command it sees, as workspace_not_initialized's remediation records.
		// The line claims only what every step behind this reason shares: the
		// next open converges from the state the step left.
		return "Retry the command: the next open runs the step the message names again from the state it left, and converges. If it fails the same way again, surface it to the user as blocking."
	case reasonMigrationPartiallyApplied:
		// The message names the snapshot as a fact; this line is the one place
		// that says to restore it. [LAW:one-source-of-truth]
		return "Do not retry, and run no other lit command first: every command opens the store and runs this migration again on top of its partial changes. Run `lit snapshots restore <name>` with the snapshot the message above names — it holds the workspace as it was just before this open migrated it, and restore moves the current store aside rather than deleting it — then rerun the command."
	case reasonSchemaShapeRefused:
		return "Do not retry unchanged — this workspace's stored shape is one lit does not converge on its own, and every command that opens the store repeats this refusal. The message above names the shape. " + agentInstructionsOpen + "Repairing a stored shape by hand is the user's call: surface this refusal to the user as blocking." + agentInstructionsClose
	case reasonWorkspaceWriteBlocked:
		// Action-only: the error line above already states the diagnosis (another
		// process holds the store). No process hunt: a command-line pattern
		// cannot tell lit's holder from any other process whose command line
		// contains "lit", and terminating the wrong one is the user's to answer
		// for. [LAW:one-source-of-truth]
		return "Wait a moment and retry: a normal command releases the store in well under a second. If it fails the same way again, surface it to the user as blocking."
	case reasonScopeExhausted:
		// Stands where the message cannot: the message names what stops the
		// scope and each route it has, but not that a bare retry changes
		// nothing. No agent-instructions envelope — the envelopes elsewhere mean
		// "mechanical, run it without asking", and deciding what unblocks a
		// scope is the opposite of mechanical.
		return "Do not retry unchanged — routing is deterministic and repeats this answer until the work named above moves. Act on what the message names — the route it offers, or the rows off your focus path — or finish or hand off what you already hold."
	case reasonScopeNarrowed:
		// The message names the ticket and the flags; what it cannot say is
		// that the answer is a property of the question, so a retry with the
		// same flags gets it again.
		return "Do not retry unchanged — routing is deterministic and repeats this answer while the filter stays. Run `lit next` without the filter, as the message says, to be handed the ticket it names."
	case reasonNoReadyWork:
		// NoWork carries the rows the pool walk went past, and the message names
		// them — so what is left here is the act for each, and the standing rule
		// that this line must never assert which situation it was: "`lit new`
		// adds work" under a message that just said the backlog is not empty is
		// exactly the remediation-contradicts-message defect.
		// [LAW:no-silent-failure]
		//
		// The lead obeys that rule too, which is why it claims determinism and
		// not startability: withheldByScope names every scope-excluded row
		// off-path without ever running capacityFor on it, so "nothing here is
		// startable" is a verdict this line has no reading to support — and
		// NoWork.Error() already declines to make it. [LAW:one-source-of-truth]
		//
		// The last clause names `lit backlog --all` rather than `lit backlog`
		// because "the whole queue" has to be true on every path that reaches
		// it. Its "Otherwise" excludes the run a focus label narrowed, but not a
		// workspace where one is SET: `lit next --all` bypasses the scope for
		// its own run and lands here with focus still on, where a bare
		// `lit backlog` prints the path and not the queue it was promised.
		// `--all` on an unfocused workspace resolves to the same scope the
		// workspace already has, so the flag costs that reader nothing.
		return "Do not retry unchanged — routing is deterministic and repeats this answer until something in the backlog moves. That is the backlog's state, not a fault. If `--type`, `--labels`, `--assignee`, or `--status` narrowed this run, drop the filter and ask again. If a `focus` label narrowed it, `lit next --all` routes over the whole queue for one run and `lit label rm <id> focus` lifts the scope. Otherwise `lit backlog --all` shows the whole queue and who holds what, and `lit new` adds work if it is genuinely empty."
	case reasonStateAlreadyHolds:
		// No act to name, because there is none: the caller asked for a state
		// the workspace is already in. It must still say "do not retry":
		// without it, an unattended agent loops on a state nothing it runs can
		// change. Anything about how the state was reached is the message's to
		// say, and not every message has it to say, so this line neither
		// restates it nor promises it. [LAW:one-source-of-truth]
		return "No action is needed — the command asked for a state the workspace is already in. Do not retry: running it again cannot change the answer, and `lit doctor` has nothing to diagnose because nothing is broken."
	case reasonTakeoverUnconfirmed:
		// Action-only, like its neighbours: the message names the holder. Both
		// ways out are named and neither is urged, and the envelope says whose
		// call the takeover is — an agent acts on this line, and --take
		// overrides another checkout's live work.
		return "Rerun with `--take` to take the lane over, or run `lit next` for work nobody else holds. " + agentInstructionsOpen + "Taking over a lane another checkout holds right now overrides that checkout's work: pass `--take` only when the user directs the takeover." + agentInstructionsClose
	case reasonPurgeUnconfirmed:
		// The reversible way out is named beside the flag, and the envelope says
		// whose call a purge is: it cannot be undone and reaches every store.
		return "Rerun with `--yes` to purge it, or run `lit delete <id>` to remove it reversibly. " + agentInstructionsOpen + "A purge cannot be undone and reaches every store lit syncs with: pass `--yes` only when the user directs the purge." + agentInstructionsClose
	case reasonOutsideGitWorkspace:
		return "Run the command inside a git repository/worktree with links initialized."
	case reasonWorkspaceNotInitialized:
		// Action-only, like its neighbours: the message above already names the
		// condition and the command that resolves it, so what this line adds is
		// that the condition is terminal and what the two ways out of it are.
		//
		// `lit doctor` is not named even to dismiss it. The default remediation
		// this reason displaces sends agents there, at a workspace that does
		// not exist for doctor to read; an agent that skims reads the command,
		// not the negation around it, so the honest way to stop sending it is
		// to leave the string out.
		//
		// No agent-instructions envelope, deliberately. The envelopes elsewhere
		// mean "mechanical, run it without asking"; `lit init` writes a
		// workspace and an agents section into someone's repository, which is
		// the repo owner's decision, not a step an agent takes on its own to
		// get itself unblocked.
		// The claim is about *this* command, not about every command. Not every
		// store-touching command repeats this answer until a workspace exists:
		// the write paths bootstrap one. Remediation is the surface an agent
		// acts on, so a convenient overstatement here is a defect.
		// [LAW:no-silent-failure]
		return "Do not retry unchanged — this repository has no lit workspace, and retrying this command cannot create one. Run `lit init` here to create it, or change to a directory that already has one."
	case reasonWorkspaceSchemaAhead:
		// The message names the supported path and, when a snapshot exists,
		// the lossy rollback, so this line defers to it rather than restating
		// either. [LAW:one-source-of-truth] Both paths change more than this
		// command: installing a lit replaces it for every workspace on the
		// machine, and the rollback discards data. So the agent is told to take
		// neither on its own, the way takeover_unconfirmed gates `--take`. The
		// rollback is named conditionally because the message offers it only
		// when a recovery snapshot exists.
		return "Do not retry unchanged — this lit is older than the schema it was asked to operate on, this workspace's or the remote's it syncs with, and rerunning the command with the same binary repeats this refusal. The message above names the supported path. " + agentInstructionsOpen + installReplacesLit + ", and any snapshot rollback the message offers discards data: take such a path only when the user directs it, and until then surface this refusal to the user as blocking." + agentInstructionsClose
	case reasonMigrationQuarantined:
		// The message names both ways out, so this line defers to it rather
		// than restating either. [LAW:one-source-of-truth] It does say which
		// snapshot: one taken after the failure still holds the quarantine
		// row. Neither way is the agent's to take on its own: a restore
		// discards what was written since the snapshot, and clearing the row
		// lets a migration that already failed run again, on the judgment that
		// the failure was transient. So both are gated the way
		// workspace_schema_ahead gates its paths.
		return "Do not retry unchanged — a migration this workspace has pending failed and is quarantined, and every command that opens the store refuses until the quarantine is cleared or a snapshot taken before the failure is restored. The message above names both. " + agentInstructionsOpen + "Restoring a snapshot discards everything written since it was taken, and clearing the quarantine lets a migration that already failed run again: take either path only when the user directs it, and until then surface this refusal to the user as blocking." + agentInstructionsClose
	case reasonImportResetFailed:
		return "Do not re-run the file unchanged — the writes the message names are still committed, and running the file again creates its created issues a second time. To finish the import, delete the issues it names as created with `lit delete`, or drop their documents from the file, then fix what refused the file and import it again: its other writes are restated as the file states them. To abandon it, put back each write the message names."
	case reasonBulkPartialFailure:
		// No re-run of the whole command is advised: it applies every landed
		// item again, an item a rule refused repeats its refusal, and one whose
		// state already holds has nothing to do. Each item's own reason says
		// what it calls for, a stopped run's stopping item is named as landed,
		// and the ids it never reached are named as such. [LAW:no-silent-failure]
		return "Do not re-run the command unchanged — any item it does not list landed, as did any it names as landed, and running it again applies those items again. Each listed item names beside its id the reason its single command answers with; act on each as that reason calls for, and re-run with only the ids that still need it: an item refused by a rule repeats its refusal until what refused it changes, an item whose state already holds needs nothing, and an item that met a fault can be re-run once the fault clears. A run that stopped because stdout refused a landed item's report names the ids it never reached; re-run with only those once stdout can take the output."
	default:
		return "Retry the command. If it still fails, run `lit doctor` for diagnostics."
	}
}
