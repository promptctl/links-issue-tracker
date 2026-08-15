package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// Command labels the reconcile handlers use as their syncTraceRecord.Command —
// one constant per handler, reused at every trace call site inside it, so a
// trace and its command name can never drift apart. resolve reuses
// proseResolveCommand (prose_pending.go), the one place that command name is
// already spelled. [LAW:one-source-of-truth]
const (
	reconcileShowCommand    = "lit sync reconcile"
	reconcileCombineCommand = "lit sync reconcile combine"
)

// reconcileFamily routes the explicit reconcile surface. The bare `lit sync
// reconcile` runs the reconcile and surfaces any prose divergence; `resolve`
// finalizes it with the agent's merged text; `abort` leaves the clone diverged.
// [LAW:decomposition] Running/surfacing, finalizing, and deferring are three
// distinct acts, each its own handler.
var reconcileFamily = commandFamily[syncRunFn]{
	usage: "usage: lit sync reconcile [resolve --resolve ID:FIELD:FINGERPRINT=TEXT ... | abort | take local|remote | combine]",
	subcommands: []subcommandRow[syncRunFn]{
		{name: "resolve", payload: runSyncReconcileResolve},
		{name: "abort", payload: runSyncReconcileAbort},
		{name: "take", payload: runSyncReconcileTake},
		{name: "combine", payload: runSyncReconcileCombine},
	},
}

// runSyncReconcile dispatches the reconcile family. A first argument naming a
// subcommand routes to it; anything else (no argument, or a leading flag) is the
// bare run-and-surface action. [LAW:dataflow-not-control-flow] The presence of a
// subcommand name selects the handler; the bare path is the default, not a
// special case threaded through a flag.
func runSyncReconcile(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		run, err := reconcileFamily.resolve(args)
		if err != nil {
			return err
		}
		return run(ctx, stdout, ws, syncStore, args[1:])
	}
	return runSyncReconcileShow(ctx, stdout, ws, syncStore, args)
}

// guardReconcileInput rejects a stray positional argument: every reconcile
// input is a flag, so a positional is a malformed command, never silently
// ignored. [LAW:no-silent-failure] [LAW:single-enforcer] the three handlers
// enforce this through one guard.
func guardReconcileInput(fs *cobraFlagSet, cmd string) error {
	if fs.NArg() != 0 {
		return UsageError{Message: fmt.Sprintf("%s takes no positional arguments; got %q", cmd, fs.Arg(0))}
	}
	return nil
}

// runSyncReconcileShow runs the field-aware reconcile and reports the outcome. A
// settled divergence linearizes transparently; a prose divergence renders the
// full guidance to stdout and returns a MergeConflictError so the command exits
// ExitConflict — the explicit counterpart to the inline auto-reconcile's passive
// nudge. [LAW:no-silent-failure] An unresolved divergence is a conflict, surfaced
// with the guidance that resolves it, never a silent success.
func runSyncReconcileShow(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync reconcile")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if err := guardReconcileInput(fs, "sync reconcile"); err != nil {
		return err
	}
	remote, branch, ok, err := freshReconcileTarget(ctx, syncStore, ws)
	if err != nil {
		recordSyncCommandTrace(ws, reconcileShowCommand, "error", err, nil)
		return err
	}
	if !ok {
		recordSyncCommandTrace(ws, reconcileShowCommand, "nothing_to_reconcile", nil, nil)
		_, writeErr := fmt.Fprintln(stdout, "nothing to reconcile: no remote with shared ticket history yet")
		return writeErr
	}
	result, err := syncStore.SyncReconcile(ctx, remote, branch)
	if err != nil {
		recordSyncCommandTrace(ws, reconcileShowCommand, "error", err, map[string]string{"remote": remote, "sync_branch": branch})
		return asSyncFailure(err)
	}
	return reportReconcileResult(ctx, stdout, ws, reconcileShowCommand, remote, branch, result, false)
}

// runSyncReconcileResolve finalizes a prose-pending reconcile with the agent's
// merged text. The resolutions must cover the live divergence exactly; if they no
// longer match (it changed, or is partial), the store returns prose-pending with
// the CURRENT conflicts, which this re-surfaces. [LAW:no-silent-failure]
func runSyncReconcileResolve(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync reconcile resolve")
	resolveValues := fs.StringArray("resolve", "Merged text for one diverged field, as ISSUE_ID:FIELD:FINGERPRINT=TEXT (repeat for every pending field)")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if err := guardReconcileInput(fs, "sync reconcile resolve"); err != nil {
		return err
	}
	if len(*resolveValues) == 0 {
		return UsageError{Message: "sync reconcile resolve needs at least one --resolve ID:FIELD:FINGERPRINT=TEXT"}
	}
	resolutions, err := parseProseResolutions(*resolveValues)
	if err != nil {
		return err
	}
	remote, branch, ok, err := freshReconcileTarget(ctx, syncStore, ws)
	if err != nil {
		recordSyncCommandTrace(ws, proseResolveCommand, "error", err, nil)
		return err
	}
	if !ok {
		recordSyncCommandTrace(ws, proseResolveCommand, "nothing_to_reconcile", nil, nil)
		_, writeErr := fmt.Fprintln(stdout, "nothing to reconcile: no remote with shared ticket history yet")
		return writeErr
	}
	result, err := syncStore.SyncReconcileResolved(ctx, remote, branch, resolutions)
	if err != nil {
		recordSyncCommandTrace(ws, proseResolveCommand, "error", err, map[string]string{"remote": remote, "sync_branch": branch})
		return asSyncFailure(err)
	}
	return reportReconcileResult(ctx, stdout, ws, proseResolveCommand, remote, branch, result, true)
}

// runSyncReconcileAbort defers the reconcile: the clone stays diverged and usable.
// There is nothing to roll back — the prose-pending state is never staged or
// persisted, so leaving it is the whole "discard". [LAW:no-silent-failure] This is
// the clean exit-zero escape the agent takes when it chooses to escalate to the
// user instead of merging inline, distinct from the unresolved state's
// ExitConflict.
func runSyncReconcileAbort(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync reconcile abort")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if err := guardReconcileInput(fs, "sync reconcile abort"); err != nil {
		return err
	}
	// abort is a real decision — the agent choosing to defer/escalate rather
	// than merge inline — not merely a usage no-op, so it gets the same durable
	// trace its three siblings (resolve/take/combine) do. [LAW:no-silent-failure]
	recordSyncCommandTrace(ws, "lit sync reconcile abort", "aborted", nil, nil)
	_, err := fmt.Fprintln(stdout, "reconcile deferred: the clone remains diverged and usable; a later command re-surfaces the divergence, or run `lit sync reconcile` when ready")
	return err
}

// runSyncReconcileTake resolves an unrelated-history divergence by taking one side
// wholesale. The side is a required positional — `local` or `remote` — mapped to the
// store resolution value; anything else is a usage error. The chosen side survives and
// the OTHER side's unique issues are discarded BY DESIGN, which the outcome names
// explicitly rather than dropping silently. [LAW:no-silent-failure]
//
// The take is the epic's "agent-mediated destruction" path, so it runs only with
// the owner's approval (links-sync-pgct.4): without a matching --owner-approved
// token the store's gate refuses, and this surfaces the refusal block naming what
// the take would destroy and how the owner authorizes it.
func runSyncReconcileTake(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync reconcile take")
	ownerApproved := fs.String("owner-approved", "", "Owner-issued approval token for this exact divergence and side (printed by the refusal this command gives without it)")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return UsageError{Message: "sync reconcile take needs exactly one side: 'local' (keep your backlog) or 'remote' (adopt theirs)"}
	}
	choice, err := parseUnrelatedSide(fs.Arg(0))
	if err != nil {
		return err
	}
	// The chosen side is part of what command ran — "took local" and "took remote"
	// are different decisions, so the trace command names which. [LAW:one-source-of-truth]
	command := "lit sync reconcile take " + string(choice)
	remote, branch, ok, err := freshReconcileTarget(ctx, syncStore, ws)
	if err != nil {
		recordSyncCommandTrace(ws, command, "error", err, nil)
		return err
	}
	if !ok {
		recordSyncCommandTrace(ws, command, "nothing_to_reconcile", nil, nil)
		_, writeErr := fmt.Fprintln(stdout, "nothing to reconcile: no remote with shared ticket history yet")
		return writeErr
	}
	result, err := syncStore.SyncResolveUnrelated(ctx, remote, branch, choice, strings.TrimSpace(*ownerApproved))
	if err != nil {
		var approval store.OwnerApprovalRequiredError
		if errors.As(err, &approval) {
			// The gate firing is a real decision, durably traced like its sibling
			// outcomes; the refusal also re-surfaces the unrelated-histories state,
			// so the owner hears about the fork an agent just tried to resolve
			// destructively (deduplicated with the detection-time notification).
			// [LAW:no-silent-failure]
			recordSyncTraceLogged(ws, syncTraceRecord{
				Command:   command,
				Decision:  "owner_approval_required",
				Status:    "ok",
				Reason:    approval.Error(),
				BuildNote: resolveBuildStatusNote(time.Now()),
				Metadata:  map[string]string{"remote": remote, "sync_branch": branch},
			})
			if ev, ok := ownerNotifyEventForFailure(SyncFailure{
				Class:  syncFailureUnrelatedHistories,
				Remote: remote,
				Branch: branch,
			}); ok {
				maybeNotifyOwner(ctx, ws, ev)
			}
			return ownerApprovalRefusalError{Approval: approval, Remote: remote, Branch: branch}
		}
		recordSyncCommandTrace(ws, command, "error", err, map[string]string{"remote": remote, "sync_branch": branch})
		return asSyncFailure(err)
	}
	return reportTakeOutcome(stdout, ws, command, remote, branch, result)
}

// runSyncReconcileCombine resolves an unrelated-history divergence by COMBINING both
// sides: the union of every issue, with ids present on both field-merged against no base.
// It surfaces an on-both prose conflict for inline resolution exactly as the three-way
// reconcile does (the SAME `lit sync reconcile resolve` finalizes it), so no shared-id
// free-text is ever auto-picked and no unique issue is dropped. [LAW:no-silent-failure]
func runSyncReconcileCombine(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync reconcile combine")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if err := guardReconcileInput(fs, "sync reconcile combine"); err != nil {
		return err
	}
	remote, branch, ok, err := freshReconcileTarget(ctx, syncStore, ws)
	if err != nil {
		recordSyncCommandTrace(ws, reconcileCombineCommand, "error", err, nil)
		return err
	}
	if !ok {
		recordSyncCommandTrace(ws, reconcileCombineCommand, "nothing_to_reconcile", nil, nil)
		_, writeErr := fmt.Fprintln(stdout, "nothing to reconcile: no remote with shared ticket history yet")
		return writeErr
	}
	result, err := syncStore.SyncReconcileCombine(ctx, remote, branch)
	if err != nil {
		recordSyncCommandTrace(ws, reconcileCombineCommand, "error", err, map[string]string{"remote": remote, "sync_branch": branch})
		return asSyncFailure(err)
	}
	return reportReconcileResult(ctx, stdout, ws, reconcileCombineCommand, remote, branch, result, false)
}

// parseUnrelatedSide maps the take command's positional to the store resolution
// value. The accepted words match the inventory the operator just read (`only on
// local` / `only on remote`), so the choice names the same side the visibility does.
// [LAW:no-silent-failure] an unrecognized side is a usage error, never a silent default.
func parseUnrelatedSide(arg string) (store.UnrelatedResolution, error) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "local":
		return store.TakeLocal, nil
	case "remote":
		return store.TakeRemote, nil
	default:
		return "", UsageError{Message: fmt.Sprintf("sync reconcile take: unknown side %q; want 'local' or 'remote'", arg)}
	}
}

// takeReasonForState maps a take outcome's three legal states to the trace
// reason `reportTakeOutcome` records, or "" for anything else (the caller's
// signal to treat it as the unexpected-state bug). [LAW:one-source-of-truth]
// one mapping for this command, separate from reconcileReasonForState's
// automatic-reconcile phrasing, which does not fit an explicit `take`.
func takeReasonForState(state store.SyncReconcileState) string {
	switch state {
	case store.SyncReconcileTookRemote:
		return "took remote: the local backlog now equals the remote; local-only issues discarded by design"
	case store.SyncReconcileTookLocal:
		return "took local: the local backlog's commits were replayed onto the remote head with their provenance; remote-only issues discarded by design"
	case store.SyncReconcileNotDiverged:
		return "the clone is not diverged from the remote; nothing to reconcile"
	default:
		return ""
	}
}

// reconcileCommandReasonForState is reconcileReasonForState's counterpart for
// reportReconcileResult, the one output path for the three explicit,
// user-initiated reconcile commands (`lit sync reconcile`, `resolve`,
// `combine`) — never the inline auto-reconcile reconcileReasonForState was
// written for. Reusing that mapping here would attribute an explicit command
// to automation (e.g. a `lit sync reconcile combine` trace reading "automatic
// reconcile completed with state combined"), the same misattribution
// takeReasonForState already exists to avoid for `take`. [LAW:one-source-of-truth]
func reconcileCommandReasonForState(state store.SyncReconcileState) string {
	switch state {
	case store.SyncReconcileLinearized:
		return "reconciled: the divergence merged into linear history"
	case store.SyncReconcileProsePending:
		return "every field resolved but free-text diverged on both sides; held for inline merge"
	case store.SyncReconcileCombined:
		return "combined: unioned both backlogs, replaying the local commits with their provenance"
	case store.SyncReconcileNotDiverged:
		return "the clone is not diverged from the remote; nothing to reconcile"
	default:
		return "reconcile completed with state " + string(state)
	}
}

// reportTakeOutcome renders a take-one resolution. Each successful take names the
// discarded side's unique issues from the both-sides inventory, so the operator sees
// exactly what was dropped — the discard is reported, never silent. A not-diverged
// result is the benign no-op (the divergence already cleared); any other state from a
// take is a bug, surfaced rather than rendered as a bland success. [LAW:no-silent-failure]
// It records the durable, unconditional trace for the take decision before
// rendering — the one point every take outcome (success or bug) passes through.
// [LAW:single-enforcer]
func reportTakeOutcome(stdout io.Writer, ws workspace.Info, command string, remote, branch string, result store.SyncReconcileResult) error {
	metadata := map[string]string{"remote": remote, "sync_branch": branch}
	decision := string(result.State)
	status := "ok"
	// [LAW:one-source-of-truth] a dedicated mapping, not reconcileReasonForState:
	// that one's every phrase leads with "automatic reconcile", which is accurate
	// for the inline auto-reconcile it was written for but false here — `lit sync
	// reconcile take <side>` is a command the agent explicitly ran.
	reason := takeReasonForState(result.State)
	if reason == "" {
		decision = "error"
		status = "error"
		reason = fmt.Sprintf("unexpected result state %q", result.State)
	}
	recordSyncTraceLogged(ws, syncTraceRecord{
		Command:   command,
		Decision:  decision,
		Status:    status,
		Reason:    reason,
		BuildNote: resolveBuildStatusNote(time.Now()),
		Metadata:  metadata,
	})
	ref := remote + "/" + branch
	switch result.State {
	case store.SyncReconcileTookRemote:
		clearOwnerNotify(ws, ownerNotifyDivergenceKinds...)
		_, err := fmt.Fprintf(stdout,
			"took remote: the local backlog now equals %s and sync is clean (no push needed).\nDISCARDED the local-only issue(s), by design: %s\n",
			ref, describeIDSet(discardedIDs(result.Unrelated, store.TakeRemote)))
		return err
	case store.SyncReconcileTookLocal:
		clearOwnerNotify(ws, ownerNotifyDivergenceKinds...)
		_, err := fmt.Fprintf(stdout,
			"took local: your backlog now sits on top of %s — %s replayed with original messages and timestamps; run `lit sync push` (or let auto-sync) to fast-forward the remote onto it.\nDISCARDED the remote-only issue(s), by design: %s\n",
			ref, describeReplayed(result.Replayed), describeIDSet(discardedIDs(result.Unrelated, store.TakeLocal)))
		return err
	case store.SyncReconcileNotDiverged:
		clearOwnerNotify(ws, ownerNotifyDivergenceKinds...)
		_, err := fmt.Fprintln(stdout, "nothing to reconcile: the clone is not diverged from the remote")
		return err
	default:
		return fmt.Errorf("sync reconcile take: unexpected result state %q — this is a bug; please report it", result.State)
	}
}

// describeReplayed phrases the provenance-replay count for the fold outcomes:
// how many of the folded side's commits landed individually ahead of the marker
// commit. Zero is a real outcome (every folded change was already contained in
// the spine), so it renders explicitly rather than vanishing.
func describeReplayed(count int) string {
	if count == 1 {
		return "1 local commit"
	}
	return fmt.Sprintf("%d local commits", count)
}

// discardedIDs is the side the take drops: taking remote discards the local-only
// issues, taking local discards the remote-only issues. A nil inventory (defensively)
// yields no ids, which describeIDSet renders as an explicit "(0)".
func discardedIDs(inv *store.UnrelatedInventory, choice store.UnrelatedResolution) []string {
	if inv == nil {
		return nil
	}
	switch choice {
	case store.TakeRemote:
		return inv.OnlyLocal
	case store.TakeLocal:
		return inv.OnlyRemote
	default:
		return nil
	}
}

// reportReconcileResult renders a reconcile outcome. A prose-pending result prints
// the guidance and returns a MergeConflictError so the command exits ExitConflict;
// an unrelated-histories result returns the one sync-failure contract (also exit
// ExitConflict), so `lit sync reconcile`, `lit sync pull`, and the inline receive
// all surface no-common-ancestor identically; every other state is a one-line
// success. resolved=true distinguishes a finalize whose resolutions missed the live
// divergence (re-surfaced) from a first-time surface, so the agent knows to re-merge
// the CURRENT conflicts shown. It records the durable, unconditional trace for the
// reconcile decision — the one point every one of the three callers' outcomes
// passes through — and it feeds the owner channel the same way every surface does:
// a held state notifies out-of-band, a converged one ends the divergence episode
// (links-sync-pgct.4). [LAW:single-enforcer]
func reportReconcileResult(ctx context.Context, stdout io.Writer, ws workspace.Info, command string, remote, branch string, result store.SyncReconcileResult, resolved bool) error {
	metadata := map[string]string{"remote": remote, "sync_branch": branch}
	switch result.State {
	case store.SyncReconcileUnrelated:
		// [LAW:single-enforcer] one contract, every surface — the block is the error's
		// message, printed by the top-level sink, so no separate stdout write here (as
		// with a held prose conflict on `lit sync pull`). Age is unknown at this surface,
		// which the unrelated escalation does not use (its severity is fixed, not aged).
		failure := SyncFailureError{Failure: SyncFailure{
			Class:     syncFailureUnrelatedHistories,
			Remote:    remote,
			Branch:    branch,
			Ahead:     result.Ahead,
			Behind:    result.Behind,
			Inventory: result.Unrelated,
			BuildNote: resolveBuildStatusNote(time.Now()),
		}}
		recordSyncHeldTrace(ws, command, failure, metadata)
		if ev, ok := ownerNotifyEventForFailure(failure.Failure); ok {
			maybeNotifyOwner(ctx, ws, ev)
		}
		return failure
	case store.SyncReconcileProsePending:
		metadata["pending"] = strconv.Itoa(len(result.Pending))
		// Resolved once and reused below for renderProsePendingGuidance — this
		// branch is the one place a reconcile decision needs the build note twice
		// in the same call, so it bypasses recordReconcileDecisionTrace (which
		// would resolve its own) rather than pay version.Get()'s embedded-FS read
		// a second time for the same value. [LAW:carrying-cost]
		buildNote := resolveBuildStatusNote(time.Now())
		recordSyncTraceLogged(ws, syncTraceRecord{
			Command:   command,
			Decision:  string(result.State),
			Status:    "ok",
			Reason:    reconcileCommandReasonForState(result.State),
			BuildNote: buildNote,
			Metadata:  metadata,
		})
		if resolved {
			if _, err := fmt.Fprintln(stdout, "the divergence changed since you read it; your resolutions were not applied. Re-merge the CURRENT conflicts below:"); err != nil {
				return err
			}
		}
		// A held prose state on the explicit reconcile can be the FIRST detection
		// (auto-sync disabled), so it notifies like every other surface, de-duplicated
		// against the inline receive's earlier detection when there was one.
		if ev, ok := ownerNotifyEventForFailure(SyncFailure{
			Class:  syncFailureProseHeld,
			Remote: remote,
			Branch: branch,
			Fields: result.Pending,
		}); ok {
			maybeNotifyOwner(ctx, ws, ev)
		}
		if err := renderProsePendingGuidance(stdout, result.Pending, buildNote); err != nil {
			return err
		}
		return MergeConflictError{Message: fmt.Sprintf("reconcile holds %d free-text field(s) for inline merge; run `%s` with your merged text", len(result.Pending), proseResolveCommand)}
	case store.SyncReconcileLinearized:
		recordReconcileDecisionTrace(ws, command, result.State, metadata)
		clearOwnerNotify(ws, ownerNotifyDivergenceKinds...)
		_, err := fmt.Fprintf(stdout, "reconciled: the divergence merged into linear history — %s replayed with original messages and timestamps; the next push fast-forwards\n", describeReplayed(result.Replayed))
		return err
	case store.SyncReconcileCombined:
		recordReconcileDecisionTrace(ws, command, result.State, metadata)
		clearOwnerNotify(ws, ownerNotifyDivergenceKinds...)
		// Report what the union KEPT from each side, so "nothing dropped" is evidenced, not
		// asserted: the both-sides partition names the kept-local, kept-remote, and
		// field-merged shared ids. A defensively-absent inventory reads as empty sides, which
		// describeIDSet renders as explicit "(0)". [LAW:no-silent-failure] [FRAMING:representation]
		inv := result.Unrelated
		if inv == nil {
			inv = &store.UnrelatedInventory{}
		}
		_, err := fmt.Fprintf(stdout,
			"combined: unioned both backlogs onto %s — %s replayed with original messages and timestamps; run `lit sync push` (or let auto-sync) to fast-forward the remote onto it.\n"+
				"  kept local-only:  %s\n"+
				"  kept remote-only: %s\n"+
				"  field-merged on both: %s\n",
			remote+"/"+branch,
			describeReplayed(result.Replayed),
			describeIDSet(inv.OnlyLocal),
			describeIDSet(inv.OnlyRemote),
			describeIDSet(inv.OnBoth))
		return err
	case store.SyncReconcileNotDiverged:
		recordReconcileDecisionTrace(ws, command, result.State, metadata)
		clearOwnerNotify(ws, ownerNotifyDivergenceKinds...)
		_, err := fmt.Fprintln(stdout, "nothing to reconcile: the clone is not diverged from the remote")
		return err
	default:
		// An unrecognized state must not trace as a bland "ok" decision — it is the
		// same class of bug buildSyncPullPayload's "unknown" status guards against.
		// [LAW:no-silent-failure]
		recordSyncTraceLogged(ws, syncTraceRecord{
			Command:   command,
			Decision:  "error",
			Status:    "error",
			Reason:    fmt.Sprintf("unrecognized reconcile result state %q", result.State),
			BuildNote: resolveBuildStatusNote(time.Now()),
			Metadata:  metadata,
		})
		_, err := fmt.Fprintf(stdout, "reconcile completed with state %s\n", result.State)
		return err
	}
}

// recordReconcileDecisionTrace records the durable, unconditional trace shared
// by every reconcile outcome that completed and settled on a definite state
// (as opposed to SyncReconcileUnrelated, which is agent-actionable and routes
// through recordSyncHeldTrace with the fuller SyncFailure context instead).
// Every caller of reportReconcileResult (this function's only caller) is an
// explicit, user-initiated command, so its Reason comes from
// reconcileCommandReasonForState, not reconcileReasonForState — the latter's
// "automatic reconcile..." phrasing belongs to the inline auto-reconcile path
// alone. [LAW:one-source-of-truth] [LAW:composability] one call site for the
// four switch branches that only differ in which state they name.
func recordReconcileDecisionTrace(ws workspace.Info, command string, state store.SyncReconcileState, metadata map[string]string) {
	recordSyncTraceLogged(ws, syncTraceRecord{
		Command:   command,
		Decision:  string(state),
		Status:    "ok",
		Reason:    reconcileCommandReasonForState(state),
		BuildNote: resolveBuildStatusNote(time.Now()),
		Metadata:  metadata,
	})
}

// freshReconcileTarget fetches the latest remote and resolves the remote+branch
// the reconcile reads, so the divergence it sees is current rather than stale from
// a prior fetch. ok=false means there is nothing to reconcile against (no remote,
// an empty remote, or this branch never synced). [LAW:single-enforcer] It resolves
// the remote and branch through the same selectors push/pull/receive use, so the
// four never disagree.
func freshReconcileTarget(ctx context.Context, syncStore *store.Store, ws workspace.Info) (remote string, branch string, ok bool, err error) {
	syncState, err := syncDoltRemotesFromGit(ctx, syncStore, ws)
	if err != nil {
		return "", "", false, err
	}
	remoteName, err := resolveSyncRemote("", workspace.UpstreamRemote(ctx, ws.RootDir), syncState.gitRemotes)
	if err != nil {
		return "", "", false, err
	}
	if remoteName == "" {
		return "", "", false, nil
	}
	hasRefs, err := workspace.RemoteHasRefs(ctx, ws.RootDir, remoteName)
	if err != nil {
		return "", "", false, fmt.Errorf("check remote refs %q: %w", remoteName, err)
	}
	if !hasRefs {
		return "", "", false, nil
	}
	branchName, err := resolveSyncBranch(ctx, ws.RootDir, remoteName)
	if err != nil {
		return "", "", false, err
	}
	if err := syncStore.SyncFetch(ctx, remoteName, false); err != nil {
		return "", "", false, fmt.Errorf("fetch %q before reconcile: %w", remoteName, err)
	}
	if err := markFetchSuccess(ws); err != nil {
		fmt.Fprintf(os.Stderr, "lit: fetch-success marker not written: %v\n", err)
	}
	return remoteName, branchName, true, nil
}
