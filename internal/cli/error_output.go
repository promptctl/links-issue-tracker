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
	exitCode := ExitCode(err)
	reason := commandErrorReason(err)
	_, _ = fmt.Fprintf(stderr, "error (code=%d, reason=%s): %v\n", exitCode, reason, err)
	if remediation := commandErrorRemediation(reason); remediation != "" {
		_, _ = fmt.Fprintf(stderr, "remediation: %s\n", remediation)
	}
	return exitCode
}

// commandReason is the machine-readable reason a failed command prints beside
// its exit code. The tokens are a published contract (docs/cli-reference.md
// lists them): a script branches on them where one exit code covers failures
// that call for different acts. [LAW:types-are-the-program] each token is
// spelled once, here, so the classifier and the remediation switch cannot
// disagree on a spelling and silently fall through to the default retry advice.
type commandReason string

const (
	reasonBulkPartialFailure      commandReason = "bulk_partial_failure"
	reasonCommandFailed           commandReason = "command_failed"
	reasonCorruptionDetected      commandReason = "corruption_detected"
	reasonEntityNotFound          commandReason = "entity_not_found"
	reasonMergeConflict           commandReason = "merge_conflict"
	reasonNoReadyWork             commandReason = "no_ready_work"
	reasonOutsideGitWorkspace     commandReason = "outside_git_workspace"
	reasonOwnerApprovalRequired   commandReason = "owner_approval_required"
	reasonRemoteUnreachable       commandReason = "remote_unreachable"
	reasonRetiredCommand          commandReason = "retired_command"
	reasonScopeExhausted          commandReason = "scope_exhausted"
	reasonStateAlreadyHolds       commandReason = "state_already_holds"
	reasonStoredPrefixRefused     commandReason = "stored_prefix_refused"
	reasonSyncDivergence          commandReason = "sync_divergence"
	reasonTakeoverUnconfirmed     commandReason = "takeover_unconfirmed"
	reasonTemplateShapeRefused    commandReason = "template_shape_refused"
	reasonTransientGcContention   commandReason = "transient_gc_contention"
	reasonUnknownCommand          commandReason = "unknown_command"
	reasonUnsupportedFlag         commandReason = "unsupported_flag"
	reasonUsageError              commandReason = "usage_error"
	reasonValidationRefused       commandReason = "validation_refused"
	reasonWorkspaceBusy           commandReason = "workspace_busy"
	reasonWorkspaceNotInitialized commandReason = "workspace_not_initialized"
	reasonWorkspaceSchemaAhead    commandReason = "workspace_schema_ahead"
	reasonWorkspaceWriteBlocked   commandReason = "workspace_write_blocked"
)

// commandErrorReason maps a typed error to its machine-readable reason string.
// [LAW:single-enforcer] All error→reason mappings live here; dispatch is by type via errors.As.
// [LAW:types-are-the-program] No message text is inspected; classification is carried by the type.
func commandErrorReason(err error) commandReason {
	var notFound storage.NotFoundError
	if errors.As(err, &notFound) {
		return reasonEntityNotFound
	}
	var mergeConflict MergeConflictError
	if errors.As(err, &mergeConflict) {
		return reasonMergeConflict
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
	// An action on an epic is refused because an epic's state is its children's
	// to set. Both halves are terminal — no retry of the same command can move
	// either — so neither may reach the default's "Retry the command", which
	// would tell the agent closing a finished epic to loop on a condition that
	// cannot change. They are separate reasons because the act each calls for
	// is different: one asks for a state that already holds and wants nothing
	// done at all, the other asks for something only the children can do, and
	// a single reason could only name one of them.
	// [LAW:one-type-per-behavior]
	var containerAction model.ContainerActionError
	if errors.As(err, &containerAction) {
		if containerAction.Satisfied() {
			return reasonStateAlreadyHolds
		}
		return reasonValidationRefused
	}
	// The router's two terminal answers. Neither is a fault: routing asked a
	// well-formed question and the honest reply was "nothing to hand you". They
	// are separate reasons because the act each calls for is different — one
	// asks you to move work you already hold, the other to widen the question or
	// create work — and a single reason could only name one of them.
	// [LAW:one-type-per-behavior]
	var exhausted Exhausted
	if errors.As(err, &exhausted) {
		return reasonScopeExhausted
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
	// A workspace whose schema is ahead of this binary. Terminal for the binary
	// that raised it — no rerun changes which lit is installed — so it must not
	// reach the default's retry advice. Not validation_refused: the command is
	// not what is wrong, so "adjust the command" would be false advice.
	// [LAW:one-type-per-behavior]
	var schemaAhead *store.UnsupportedSchemaVersionError
	if errors.As(err, &schemaAhead) {
		return reasonWorkspaceSchemaAhead
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
	var bulkFailure BulkFailureError
	if errors.As(err, &bulkFailure) {
		return reasonBulkPartialFailure
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
	return reasonCommandFailed
}

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
	case reasonMergeConflict:
		return "Sync and retry after resolving conflicts."
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
	case reasonCorruptionDetected:
		return "Run `lit doctor --fix integrity` and retry. " + agentInstructionsOpen + "This command is idempotent and safe to run without confirmation." + agentInstructionsClose
	case reasonTransientGcContention:
		return "Retry once. If the error persists, run `lit doctor --fix`. " + agentInstructionsOpen + "This command is idempotent and safe to run without confirmation." + agentInstructionsClose
	case reasonWorkspaceWriteBlocked:
		// Action-only: the error line above already states the diagnosis (another
		// process holds the store), so the remediation jumps straight to the steps,
		// like every other case here. [LAW:one-source-of-truth]
		return "Wait a moment and retry — a normal command releases the store in well under a second. If it persists, a lit process is stuck: find it with `ps aux | grep '[l]it'` and terminate it, then retry; if none is running the hold is stale, so run `lit doctor --fix`. " + agentInstructionsOpen + "This is a mechanical, self-diagnosable state — the steps above resolve it without needing the user's input." + agentInstructionsClose
	case reasonScopeExhausted:
		// Stands where the message cannot: the message names what stops the
		// scope and each route it has, but not that a bare retry changes
		// nothing. No agent-instructions envelope — the envelopes elsewhere mean
		// "mechanical, run it without asking", and deciding what unblocks a
		// scope is the opposite of mechanical.
		return "Do not retry unchanged — routing is deterministic and repeats this answer until the work named above moves. Act on what the message names — the route it offers, or the rows off your focus path — or finish or hand off what you already hold."
	case reasonNoReadyWork:
		// NoWork carries the rows the pool walk went past, and the message names
		// them — so what is left here is the act for each, and the standing rule
		// that this line must never assert which situation it was: "`lit new`
		// adds work" under a message that just said the backlog is not empty is
		// exactly the remediation-contradicts-message defect.
		// [LAW:no-silent-failure]
		//
		// The lead obeys that rule too, which is why it claims determinism and
		// not startability: withheldByScope stamps every scope-excluded row
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
		// change. How the state was reached is the message's to say, not this
		// line's, so nothing here restates it. [LAW:one-source-of-truth]
		return "No action is needed — the command asked for a state the workspace is already in, and the message above says how that state was reached. Do not retry: running it again cannot change the answer, and `lit doctor` has nothing to diagnose because nothing is broken."
	case reasonTakeoverUnconfirmed:
		// Action-only, like its neighbours: the message names the holder. Both
		// ways out are named and neither is urged, and the envelope says whose
		// call the takeover is — an agent acts on this line, and --take
		// overrides another checkout's live work.
		return "Rerun with `--take` to take the lane over, or run `lit next` for work nobody else holds. " + agentInstructionsOpen + "Taking over a lane another checkout holds right now overrides that checkout's work: pass `--take` only when the user directs the takeover." + agentInstructionsClose
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
		return "Do not retry unchanged — this lit is older than the workspace's schema, and rerunning the command with the same binary repeats this refusal. The message above names the supported path. " + agentInstructionsOpen + "Installing a different lit replaces it for every workspace on this machine, and any snapshot rollback the message offers discards data: take such a path only when the user directs it, and until then surface this refusal to the user as blocking." + agentInstructionsClose
	case reasonBulkPartialFailure:
		return "Some items failed; see the per-item errors above. Re-run the command for only the failed IDs after addressing each error."
	default:
		return "Retry the command. If it still fails, run `lit doctor` for diagnostics."
	}
}
