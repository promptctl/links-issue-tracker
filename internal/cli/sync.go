package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/precedence"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

const debugSyncBranchEnvVar = "LINKS_DEBUG_DOLT_SYNC_BRANCH"

// firstPushSkipMessage is emitted when lit sync is invoked against a remote
// that advertises no refs at all. This is only a legitimate state during the
// very first push to a brand-new empty repo; in every other situation it
// indicates a real problem (wrong URL, auth failure that ls-remote didn't
// surface as an error, etc.) and must not be silently ignored.
const firstPushSkipMessage = "Skipping lit sync: remote has no refs yet. " +
	"This is normal ONLY for the very first push to a brand-new empty repo. " +
	"If you have pushed to this remote before, do NOT ignore this message — " +
	"something is wrong (check the remote URL, credentials, or run `git ls-remote <remote>`)."

// syncRunFn is the handler shape for sync subcommands: every one operates on
// the workspace's open sync store.
type syncRunFn func(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error

// withSyncStore adapts a sync handler to the workspace family shape, owning
// the sync store's open/close lifecycle so no handler manages it.
// [LAW:no-ambient-temporal-coupling]
func withSyncStore(run syncRunFn) wsRunFn {
	return func(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
		syncStore, err := store.OpenSync(ctx, ws.DatabasePath, ws.WorkspaceID)
		if err != nil {
			return err
		}
		defer syncStore.Close()
		return run(ctx, stdout, ws, syncStore, args)
	}
}

var syncFamily = commandFamily[wsRunFn]{
	usage: "usage: lit sync <status|remote|fetch|pull|push|reconcile> ...",
	subcommands: []subcommandRow[wsRunFn]{
		{name: "status", payload: withSyncStore(runSyncStatus)},
		{name: "remote", payload: withSyncStore(runSyncRemote)},
		{name: "fetch", payload: withSyncStore(runSyncFetch)},
		{name: "pull", payload: withSyncStore(runSyncPull)},
		{name: "push", payload: withSyncStore(runSyncPush)},
		{name: "reconcile", payload: withSyncStore(runSyncReconcile)},
		// Hidden: the detached on-change mirror entrypoint. Absent from `usage`
		// above, so it never shows in help; it manages its own store lifecycle
		// (wait-for-parent, then open) and so is registered without withSyncStore.
		{name: backgroundMirrorSubcommand, payload: runBackgroundMirror, hidden: true},
	},
}

var syncRemoteFamily = commandFamily[syncRunFn]{
	usage: "usage: lit sync remote ls",
	subcommands: []subcommandRow[syncRunFn]{
		{name: "ls", payload: runSyncRemoteLs},
	},
}

func runSyncRemote(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	run, err := syncRemoteFamily.resolve(args)
	if err != nil {
		return err
	}
	return run(ctx, stdout, ws, syncStore, args[1:])
}

func runSyncRemoteLs(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync remote ls")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	syncState, err := readSyncRemoteState(ctx, syncStore, ws)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(
		stdout,
		"git=%d dolt=%d added=%d updated=%d removed=%d\n",
		len(syncState.gitRemotes),
		len(syncState.doltRemotes),
		len(syncState.changes.Added),
		len(syncState.changes.Updated),
		len(syncState.changes.Removed),
	)
	return err
}

func runSyncFetch(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync fetch")
	remote := fs.String("remote", "origin", "Remote name")
	prune := fs.Bool("prune", false, "Pass --prune to dolt fetch")
	verbose := fs.Bool("verbose", false, "Include detailed remote output")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if _, err := syncDoltRemotesFromGit(ctx, syncStore, ws); err != nil {
		return err
	}
	remoteName := strings.TrimSpace(*remote)
	if err := syncStore.SyncFetch(ctx, remoteName, *prune); err != nil {
		return err
	}
	if !*verbose {
		_, err := fmt.Fprintln(stdout, "fetched")
		return err
	}
	_, err := fmt.Fprintf(stdout, "fetched %s\n", remoteName)
	return err
}

func runSyncPull(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync pull")
	remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
	verbose := fs.Bool("verbose", false, "Include detailed remote output")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	progressf("sync pull", "starting: reconciling remotes and resolving the sync source")
	syncState, err := syncDoltRemotesFromGit(ctx, syncStore, ws)
	if err != nil {
		return err
	}
	remoteName, remoteErr := resolveSyncRemote(
		strings.TrimSpace(*remote),
		workspace.UpstreamRemote(ws.RootDir),
		syncState.gitRemotes,
	)
	if remoteErr != nil {
		return remoteErr
	}
	if remoteName == "" {
		payload := map[string]any{
			"status": "skipped",
			"reason": "no_sync_remote",
			"raw":    "no upstream remote and no single configured remote; skipping sync pull",
		}
		// [LAW:dataflow-not-control-flow] exception: explicit no-remote policy requires suppressing sync side effects when remote resolution yields empty input.
		return printSyncPullPayload(stdout, payload, *verbose)
	}
	// [LAW:single-enforcer] First-push detection is centralized so pull and push share one definition of "remote is empty".
	hasRefs, refsErr := workspace.RemoteHasRefs(ws.RootDir, remoteName)
	if refsErr == nil && !hasRefs {
		payload := map[string]any{
			"status": "skipped",
			"reason": "remote_empty",
			"remote": remoteName,
			"raw":    firstPushSkipMessage,
		}
		return printSyncPullPayload(stdout, payload, *verbose)
	}
	resolvedBranch, err := resolveSyncBranch(ws.RootDir, remoteName)
	if err != nil {
		return err
	}
	progressf("sync pull", "pulling lit data from %s/%s (transfer and apply may take a moment)", remoteName, resolvedBranch)
	result, err := syncStore.SyncPull(ctx, remoteName, resolvedBranch)
	if err != nil {
		// An explicit pull is not best-effort: a fetch or reconcile failure is
		// surfaced as a command error, not swallowed the way the background
		// receive tolerates a transient hiccup. [LAW:no-silent-failure]
		return err
	}
	return printSyncPullPayload(stdout, buildSyncPullPayload(remoteName, resolvedBranch, result), *verbose)
}

func runSyncPush(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync push")
	remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
	setUpstream := fs.Bool("set-upstream", false, "Pass -u to dolt push")
	force := fs.Bool("force", false, "Pass --force to dolt push")
	verbose := fs.Bool("verbose", false, "Include detailed remote output")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	// [LAW:decomposition] The explicit `lit sync push` (and the pre-push hook it
	// backs) compacts atomically with the push; the on-change mirror pushes
	// without compaction. The choice is the push step passed as a value, so
	// performSyncPush has no compaction branch and a skipped push never compacts.
	outcome, err := performSyncPush(ctx, syncStore, ws, strings.TrimSpace(*remote), *setUpstream, *force, syncStore.SyncCompactAndPush)
	if err != nil {
		return err
	}
	// [LAW:no-silent-failure] The push error surfaces as the command's exit
	// status only after its trace has been recorded inside performSyncPush —
	// the skipped/ok payload is never printed over a failed push.
	if outcome.pushErr != nil {
		return outcome.pushErr
	}
	return printSyncPushPayload(stdout, outcome.payload(), *verbose)
}

// syncPushOutcome is the result of one push attempt, independent of CLI
// presentation. [LAW:decomposition] Reconciling remotes, resolving
// remote+branch, pushing, and recording the trace are one part; flag parsing
// and payload rendering are another. The `lit sync push` command and the
// on-change cadence owner both consume this one orchestration so their push
// behavior cannot drift. [LAW:single-enforcer]
type syncPushOutcome struct {
	status     string // "ok" | "skipped"
	reason     string // set when status == "skipped"
	remote     string
	branch     string
	message    string
	pushStatus int64
	traceRef   *automationTraceRef
	traceErr   error
	pushErr    error // the push failure; the trace is already recorded when set
}

// payload renders the outcome into the map shape printSyncPushPayload consumes.
func (o syncPushOutcome) payload() map[string]any {
	payload := map[string]any{
		"status": o.status,
		"remote": o.remote,
		"branch": o.branch,
		"raw":    o.message,
	}
	if o.status == "skipped" {
		payload["reason"] = o.reason
		return payload
	}
	payload["push_status"] = o.pushStatus
	if o.traceRef != nil {
		payload["trace_ref"] = o.traceRef.Path
	}
	if o.traceErr != nil {
		payload["trace_error"] = o.traceErr.Error()
	}
	return payload
}

// syncPushStep is the push the orchestrator runs once it has resolved the
// remote and branch — store.Store.SyncPush (push only) or
// store.Store.SyncCompactAndPush (compact + push, atomic). [LAW:dataflow-not-control-flow]
// The variant is a value the caller supplies, so performSyncPush carries no
// compaction branch and never compacts a push it skips.
type syncPushStep func(ctx context.Context, remote, branch string, setUpstream, force bool) (store.SyncPushResult, error)

// performSyncPush reconciles Dolt remotes from git, resolves the remote and
// branch, runs the supplied push step, and records an automation trace for the
// attempt. The returned error is a "could not attempt" failure (reconcile or
// remote resolution); a push that ran and failed is carried in outcome.pushErr
// with its trace already recorded, leaving the caller to decide whether that
// fails it (the command) or is best-effort (the cadence owner).
func performSyncPush(ctx context.Context, syncStore *store.Store, ws workspace.Info, remote string, setUpstream, force bool, push syncPushStep) (syncPushOutcome, error) {
	syncState, err := syncDoltRemotesFromGit(ctx, syncStore, ws)
	if err != nil {
		return syncPushOutcome{}, err
	}
	remoteName, remoteErr := resolveSyncRemote(
		strings.TrimSpace(remote),
		workspace.UpstreamRemote(ws.RootDir),
		syncState.gitRemotes,
	)
	if remoteErr != nil {
		return syncPushOutcome{}, remoteErr
	}
	if remoteName == "" {
		// [LAW:dataflow-not-control-flow] exception: explicit no-remote policy requires suppressing sync side effects when remote resolution yields empty input.
		return syncPushOutcome{
			status:  "skipped",
			reason:  "no_sync_remote",
			message: "no upstream remote and no single configured remote; skipping sync push",
		}, nil
	}
	// [LAW:single-enforcer] First-push detection is centralized so pull and push share one definition of "remote is empty".
	hasRefs, refsErr := workspace.RemoteHasRefs(ws.RootDir, remoteName)
	if refsErr == nil && !hasRefs {
		return syncPushOutcome{
			status:  "skipped",
			reason:  "remote_empty",
			remote:  remoteName,
			message: firstPushSkipMessage,
		}, nil
	}
	syncBranch, err := resolveSyncBranch(ws.RootDir, remoteName)
	if err != nil {
		return syncPushOutcome{}, err
	}
	// [LAW:dataflow-not-control-flow] Sync push runs one deterministic embedded mutation path from resolved remote+branch state.
	result, pushErr := push(ctx, remoteName, syncBranch, setUpstream, force)
	traceMetadata := map[string]string{
		"remote":      remoteName,
		"sync_branch": syncBranch,
	}
	if strings.TrimSpace(result.Message) != "" {
		traceMetadata["message"] = strings.TrimSpace(result.Message)
	}
	traceStatus := "ok"
	traceReason := "managed automation requested sync push"
	if pushErr != nil {
		traceStatus = "error"
		traceReason = pushErr.Error()
		traceMetadata["error"] = pushErr.Error()
	}
	syncCommandArgs := []string{"sync", "push", "--remote", remoteName}
	if setUpstream {
		syncCommandArgs = append(syncCommandArgs, "--set-upstream")
	}
	if force {
		syncCommandArgs = append(syncCommandArgs, "--force")
	}
	// [LAW:one-source-of-truth] Hook-triggered sync traces reuse the shared automation trace writer instead of shell-local trace formats.
	traceRef, traceRecordErr := maybeRecordAutomatedCommandTrace(
		ws,
		formatCommand(syncCommandArgs),
		"mirror Dolt data to the configured git remote",
		traceStatus,
		traceReason,
		traceMetadata,
	)
	return syncPushOutcome{
		status:     "ok",
		remote:     remoteName,
		branch:     syncBranch,
		message:    result.Message,
		pushStatus: result.Status,
		traceRef:   traceRef,
		traceErr:   traceRecordErr,
		pushErr:    pushErr,
	}, nil
}

func runSyncStatus(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync status")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	syncState, err := readSyncRemoteState(ctx, syncStore, ws)
	if err != nil {
		return err
	}
	report, err := syncStore.SyncStatus(ctx)
	if err != nil {
		return err
	}
	head := strings.TrimSpace(report.HeadCommit)
	if strings.TrimSpace(report.HeadMessage) != "" {
		head = strings.TrimSpace(report.HeadCommit + " " + report.HeadMessage)
	}
	_, err = fmt.Fprintf(
		stdout,
		"version=%v branch=%v head=%v git=%d dolt=%d added=%d updated=%d removed=%d\n",
		report.DoltVersion,
		report.Branch,
		head,
		len(syncState.gitRemotes),
		len(syncState.doltRemotes),
		len(syncState.changes.Added),
		len(syncState.changes.Updated),
		len(syncState.changes.Removed),
	)
	return err
}

func resolveSyncRemote(requestedRemote string, upstreamRemote string, gitRemotes []workspace.GitRemote) (string, error) {
	validatedRequestedRemote := strings.TrimSpace(requestedRemote)
	if validatedRequestedRemote != "" {
		// [LAW:no-silent-failure] Explicit remote that doesn't exist is a configuration error, not a skip condition.
		if !syncRemoteExists(validatedRequestedRemote, gitRemotes) {
			return "", fmt.Errorf("requested remote %q not found in configured git remotes", validatedRequestedRemote)
		}
		return validatedRequestedRemote, nil
	}
	singleRemote := ""
	if len(gitRemotes) == 1 {
		singleRemote = strings.TrimSpace(gitRemotes[0].Name)
	}
	validatedUpstreamRemote := strings.TrimSpace(upstreamRemote)
	if !syncRemoteExists(validatedUpstreamRemote, gitRemotes) {
		validatedUpstreamRemote = ""
	}
	// [LAW:one-source-of-truth] Sync remote selection is derived once from ordered candidates and shared by pull/push.
	// Candidates are trimmed where they are produced, so plain precedence suffices.
	return precedence.First(validatedUpstreamRemote, singleRemote), nil
}

func syncRemoteExists(name string, gitRemotes []workspace.GitRemote) bool {
	normalizedName := strings.TrimSpace(name)
	if normalizedName == "" {
		return false
	}
	for _, remote := range gitRemotes {
		if strings.TrimSpace(remote.Name) == normalizedName {
			return true
		}
	}
	return false
}

func resolveSyncBranch(rootDir string, remote string) (string, error) {
	debugOverride := strings.TrimSpace(os.Getenv(debugSyncBranchEnvVar))
	defaultBranch := strings.TrimSpace(workspace.DefaultRemoteBranch(rootDir, remote))
	// [LAW:single-enforcer] Sync branch selection is centralized so pull/push/hooks consume one canonical branch decision.
	resolvedBranch := precedence.First(debugOverride, defaultBranch)
	if resolvedBranch == "" {
		return "", fmt.Errorf(
			"resolve sync branch for remote %q: default branch unavailable; configure %s to override",
			strings.TrimSpace(remote),
			debugSyncBranchEnvVar,
		)
	}
	return resolvedBranch, nil
}

// buildSyncPullPayload renders a completed pull into the structured payload the
// printer consumes. The outcome variance lives entirely in the result STATE, not
// in error-string parsing: a branch the remote has never seen is the typed
// never_synced state (not a raw "not found on remote" backend string), and a
// divergence that held free text is the typed prose_pending state.
// [LAW:types-are-the-program] the state is the discriminator; [LAW:dataflow-not-control-flow]
// one builder, the state selects the fields.
func buildSyncPullPayload(remote string, branch string, result store.SyncPullResult) map[string]any {
	switch result.State {
	case store.SyncPullNeverSynced:
		return map[string]any{
			"status":        "skipped",
			"reason":        "remote_branch_missing",
			"remote":        remote,
			"branch":        branch,
			"next_command":  fmt.Sprintf("lit sync push --remote %s --set-upstream", remote),
			"retry_command": fmt.Sprintf("lit sync pull --remote %s", remote),
		}
	case store.SyncPullProsePending:
		return map[string]any{
			"status":          "prose_pending",
			"remote":          remote,
			"branch":          branch,
			"pending":         len(result.Pending),
			"resolve_command": "lit sync reconcile",
		}
	default:
		return map[string]any{
			"status": "ok",
			"state":  string(result.State),
			"remote": remote,
			"branch": branch,
		}
	}
}

func printSyncPullPayload(w io.Writer, payload map[string]any, verbose bool) error {
	status := strings.TrimSpace(fmt.Sprintf("%v", payload["status"]))
	remote := strings.TrimSpace(fmt.Sprintf("%v", payload["remote"]))
	branch := strings.TrimSpace(fmt.Sprintf("%v", payload["branch"]))
	switch status {
	case "skipped":
		reason := strings.TrimSpace(fmt.Sprintf("%v", payload["reason"]))
		if reason == "no_sync_remote" {
			if !verbose {
				return nil
			}
			_, err := fmt.Fprintln(w, "skipped sync pull: no eligible git remote")
			return err
		}
		if reason == "remote_empty" {
			// [LAW:dataflow-not-control-flow] exception: first-push skip message must always reach the caller so agents/humans see why sync did nothing.
			_, err := fmt.Fprintln(w, firstPushSkipMessage)
			return err
		}
		nextCommand := strings.TrimSpace(fmt.Sprintf("%v", payload["next_command"]))
		retryCommand := strings.TrimSpace(fmt.Sprintf("%v", payload["retry_command"]))
		if !verbose {
			_, err := fmt.Fprintf(
				w,
				"sync pull skipped; run `%s`, then retry `%s`\n",
				nextCommand,
				retryCommand,
			)
			return err
		}
		_, err := fmt.Fprintf(
			w,
			"skipped pull %s/%s: remote branch missing; run `%s`, then retry `%s`\n",
			remote,
			branch,
			nextCommand,
			retryCommand,
		)
		return err
	case "prose_pending":
		// A divergence settled every code-owned field but a free-text field
		// diverged on both sides; the reconcile held it rather than pick a side.
		// Direct the caller to the agent-resolve surface. [LAW:no-silent-failure]
		resolveCommand := strings.TrimSpace(fmt.Sprintf("%v", payload["resolve_command"]))
		_, err := fmt.Fprintf(
			w,
			"pull held a text conflict on %s/%s; resolve it with `%s`\n",
			remote,
			branch,
			resolveCommand,
		)
		return err
	default:
		if !verbose {
			_, err := fmt.Fprintln(w, "pulled")
			return err
		}
		state := strings.TrimSpace(fmt.Sprintf("%v", payload["state"]))
		if branch != "" {
			_, err := fmt.Fprintf(w, "pulled %s/%s (%s)\n", remote, branch, state)
			return err
		}
		_, err := fmt.Fprintf(w, "pulled %s (%s)\n", remote, state)
		return err
	}
}

func printSyncPushPayload(w io.Writer, payload map[string]any, verbose bool) error {
	status := strings.TrimSpace(fmt.Sprintf("%v", payload["status"]))
	raw, hasRaw := payload["raw"].(string)
	reason := strings.TrimSpace(fmt.Sprintf("%v", payload["reason"]))
	if status == "skipped" && reason == "remote_empty" {
		// [LAW:dataflow-not-control-flow] exception: first-push skip message must always reach the caller so agents/humans see why sync did nothing.
		_, err := fmt.Fprintln(w, firstPushSkipMessage)
		return err
	}
	if !verbose && status == "skipped" {
		return nil
	}
	if !verbose {
		_, err := fmt.Fprintln(w, "pushed")
		return err
	}
	if hasRaw && strings.TrimSpace(raw) != "" {
		_, err := fmt.Fprintln(w, strings.TrimSpace(raw))
		return err
	}
	if status == "skipped" {
		_, err := fmt.Fprintln(w, "skipped sync push: no eligible git remote")
		return err
	}
	remote := strings.TrimSpace(fmt.Sprintf("%v", payload["remote"]))
	branch := strings.TrimSpace(fmt.Sprintf("%v", payload["branch"]))
	if branch != "" {
		_, err := fmt.Fprintf(w, "pushed %s/%s\n", remote, branch)
		return err
	}
	_, err := fmt.Fprintf(w, "pushed %s\n", remote)
	return err
}

type remoteSyncChanges struct {
	Added   []string `json:"added"`
	Updated []string `json:"updated"`
	Removed []string `json:"removed"`
}

type remoteSyncState struct {
	gitRemotes  []workspace.GitRemote
	doltRemotes []store.SyncRemote
	changes     remoteSyncChanges
}

func readSyncRemoteState(ctx context.Context, syncStore *store.Store, ws workspace.Info) (remoteSyncState, error) {
	gitRemotes, err := workspace.GitRemotes(ws.RootDir)
	if err != nil {
		return remoteSyncState{}, fmt.Errorf("read git remotes: %w", err)
	}
	doltRemotes, err := syncStore.SyncListRemotes(ctx)
	if err != nil {
		return remoteSyncState{}, err
	}
	return remoteSyncState{
		gitRemotes:  gitRemotes,
		doltRemotes: doltRemotes,
		changes:     buildRemoteSyncChanges(gitRemotes, doltRemotes),
	}, nil
}

func syncDoltRemotesFromGit(ctx context.Context, syncStore *store.Store, ws workspace.Info) (remoteSyncState, error) {
	state, err := readSyncRemoteState(ctx, syncStore, ws)
	if err != nil {
		return remoteSyncState{}, err
	}
	gitRemotes := state.gitRemotes
	doltRemotes := state.doltRemotes
	gitByName := mapGitRemotesByName(gitRemotes)
	doltByName := mapRemotesByName(doltRemotes)
	changes := buildRemoteSyncChanges(gitRemotes, doltRemotes)

	for _, remote := range gitRemotes {
		desiredURL := store.GitBackedRemoteURL(remote.URL)
		currentURL, exists := doltByName[remote.Name]
		if !exists {
			if err := syncStore.SyncAddRemote(ctx, remote.Name, desiredURL); err != nil {
				return remoteSyncState{}, err
			}
			continue
		}
		if strings.TrimSpace(currentURL) != desiredURL {
			if err := syncStore.SyncRemoveRemote(ctx, remote.Name); err != nil {
				return remoteSyncState{}, err
			}
			if err := syncStore.SyncAddRemote(ctx, remote.Name, desiredURL); err != nil {
				return remoteSyncState{}, err
			}
		}
	}
	for name := range doltByName {
		if _, keep := gitByName[name]; keep {
			continue
		}
		if err := syncStore.SyncRemoveRemote(ctx, name); err != nil {
			return remoteSyncState{}, err
		}
	}
	finalRemotes, err := syncStore.SyncListRemotes(ctx)
	if err != nil {
		return remoteSyncState{}, err
	}
	return remoteSyncState{
		gitRemotes:  gitRemotes,
		doltRemotes: finalRemotes,
		changes:     changes,
	}, nil
}

func buildRemoteSyncChanges(gitRemotes []workspace.GitRemote, doltRemotes []store.SyncRemote) remoteSyncChanges {
	gitByName := mapGitRemotesByName(gitRemotes)
	doltByName := mapRemotesByName(doltRemotes)
	changes := remoteSyncChanges{
		Added:   []string{},
		Updated: []string{},
		Removed: []string{},
	}
	for _, remote := range gitRemotes {
		desiredURL := store.GitBackedRemoteURL(remote.URL)
		currentURL, exists := doltByName[remote.Name]
		if !exists {
			changes.Added = append(changes.Added, remote.Name)
			continue
		}
		if strings.TrimSpace(currentURL) != desiredURL {
			changes.Updated = append(changes.Updated, remote.Name)
		}
	}
	for name := range doltByName {
		if _, keep := gitByName[name]; !keep {
			changes.Removed = append(changes.Removed, name)
		}
	}
	sort.Strings(changes.Added)
	sort.Strings(changes.Updated)
	sort.Strings(changes.Removed)
	return changes
}

func mapGitRemotesByName(remotes []workspace.GitRemote) map[string]string {
	out := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		out[remote.Name] = remote.URL
	}
	return out
}

func mapRemotesByName(remotes []store.SyncRemote) map[string]string {
	out := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		name := strings.TrimSpace(remote.Name)
		url := strings.TrimSpace(remote.URL)
		if name == "" || url == "" {
			continue
		}
		out[name] = url
	}
	return out
}
