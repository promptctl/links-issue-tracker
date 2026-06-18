package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// reconcileFamily routes the explicit reconcile surface. The bare `lit sync
// reconcile` runs the reconcile and surfaces any prose divergence; `resolve`
// finalizes it with the agent's merged text; `abort` leaves the clone diverged.
// [LAW:decomposition] Running/surfacing, finalizing, and deferring are three
// distinct acts, each its own handler.
var reconcileFamily = commandFamily[syncRunFn]{
	usage: "usage: lit sync reconcile [resolve --resolve ID:FIELD:FINGERPRINT=TEXT ... | abort]",
	subcommands: []subcommandRow[syncRunFn]{
		{name: "resolve", payload: runSyncReconcileResolve},
		{name: "abort", payload: runSyncReconcileAbort},
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
		return err
	}
	if !ok {
		_, writeErr := fmt.Fprintln(stdout, "nothing to reconcile: no remote with shared ticket history yet")
		return writeErr
	}
	result, err := syncStore.SyncReconcile(ctx, remote, branch)
	if err != nil {
		return err
	}
	return reportReconcileResult(stdout, result, false)
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
		return err
	}
	if !ok {
		_, writeErr := fmt.Fprintln(stdout, "nothing to reconcile: no remote with shared ticket history yet")
		return writeErr
	}
	result, err := syncStore.SyncReconcileResolved(ctx, remote, branch, resolutions)
	if err != nil {
		return err
	}
	return reportReconcileResult(stdout, result, true)
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
	_, err := fmt.Fprintln(stdout, "reconcile deferred: the clone remains diverged and usable; a later command re-surfaces the divergence, or run `lit sync reconcile` when ready")
	return err
}

// reportReconcileResult renders a reconcile outcome. A prose-pending result prints
// the guidance and returns a MergeConflictError so the command exits ExitConflict;
// every other state is a one-line success. resolved=true distinguishes a finalize
// whose resolutions missed the live divergence (re-surfaced) from a first-time
// surface, so the agent knows to re-merge the CURRENT conflicts shown.
func reportReconcileResult(stdout io.Writer, result store.SyncReconcileResult, resolved bool) error {
	switch result.State {
	case store.SyncReconcileProsePending:
		if resolved {
			if _, err := fmt.Fprintln(stdout, "the divergence changed since you read it; your resolutions were not applied. Re-merge the CURRENT conflicts below:"); err != nil {
				return err
			}
		}
		if err := renderProsePendingGuidance(stdout, result.Pending); err != nil {
			return err
		}
		return MergeConflictError{Message: fmt.Sprintf("reconcile holds %d free-text field(s) for inline merge; run `%s` with your merged text", len(result.Pending), proseResolveCommand)}
	case store.SyncReconcileLinearized:
		_, err := fmt.Fprintln(stdout, "reconciled: the divergence merged into linear history; the next push fast-forwards")
		return err
	case store.SyncReconcileNotDiverged:
		_, err := fmt.Fprintln(stdout, "nothing to reconcile: the clone is not diverged from the remote")
		return err
	default:
		_, err := fmt.Fprintf(stdout, "reconcile completed with state %s\n", result.State)
		return err
	}
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
	remoteName, err := resolveSyncRemote("", workspace.UpstreamRemote(ws.RootDir), syncState.gitRemotes)
	if err != nil {
		return "", "", false, err
	}
	if remoteName == "" {
		return "", "", false, nil
	}
	hasRefs, err := workspace.RemoteHasRefs(ws.RootDir, remoteName)
	if err != nil {
		return "", "", false, fmt.Errorf("check remote refs %q: %w", remoteName, err)
	}
	if !hasRefs {
		return "", "", false, nil
	}
	branchName, err := resolveSyncBranch(ws.RootDir, remoteName)
	if err != nil {
		return "", "", false, err
	}
	if err := syncStore.SyncFetch(ctx, remoteName, false); err != nil {
		return "", "", false, fmt.Errorf("fetch %q before reconcile: %w", remoteName, err)
	}
	return remoteName, branchName, true, nil
}
