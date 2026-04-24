package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

var missingRemoteBranchPattern = regexp.MustCompile(`branch "([^"]+)" not found on remote`)

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

func validateSyncCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit sync <status|remote|fetch|pull|push> ...", "status", "remote", "fetch", "pull", "push")
}

func runSync(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit sync <status|remote|fetch|pull|push> ...")
	}
	syncStore, err := store.OpenSync(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return err
	}
	defer syncStore.Close()

	switch args[0] {
	case "remote":
		if len(args) < 2 {
			return errors.New("usage: lit sync remote ls [--json]")
		}
		switch args[1] {
		case "ls":
			fs := newCobraFlagSet("sync remote ls")
			jsonOut := fs.Bool("json", false, "Output JSON")
			if err := parseFlagSet(fs, args[2:], stdout); err != nil {
				return err
			}
			syncState, err := readSyncRemoteState(ctx, syncStore, ws)
			if err != nil {
				return err
			}
			payload := map[string]any{
				"git_remotes":  syncState.gitRemotes,
				"dolt_remotes": syncState.doltRemotes,
				"changes":      syncState.changes,
			}
			return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
				p := v.(map[string]any)
				_, err := fmt.Fprintf(
					w,
					"git=%d dolt=%d added=%d updated=%d removed=%d\n",
					len(p["git_remotes"].([]workspace.GitRemote)),
					len(p["dolt_remotes"].([]store.SyncRemote)),
					len(syncState.changes.Added),
					len(syncState.changes.Updated),
					len(syncState.changes.Removed),
				)
				return err
			})
		default:
			return errors.New("usage: lit sync remote ls [--json]")
		}
	case "fetch":
		fs := newCobraFlagSet("sync fetch")
		remote := fs.String("remote", "origin", "Remote name")
		prune := fs.Bool("prune", false, "Pass --prune to dolt fetch")
		verbose := fs.Bool("verbose", false, "Include detailed remote output")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		if _, err := syncDoltRemotesFromGit(ctx, syncStore, ws); err != nil {
			return err
		}
		remoteName := strings.TrimSpace(*remote)
		if err := syncStore.SyncFetch(ctx, remoteName, *prune); err != nil {
			return err
		}
		payload := map[string]any{
			"status": "ok",
			"remote": remoteName,
			"prune":  *prune,
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]any)
			if !*verbose {
				_, err := fmt.Fprintln(w, "fetched")
				return err
			}
			_, err := fmt.Fprintf(w, "fetched %s\n", p["remote"])
			return err
		})
	case "pull":
		fs := newCobraFlagSet("sync pull")
		remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
		verbose := fs.Bool("verbose", false, "Include detailed remote output")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
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
			return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
				return printSyncPullPayload(w, v, *verbose)
			})
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
			return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
				return printSyncPullPayload(w, v, *verbose)
			})
		}
		resolvedBranch, err := resolveSyncBranch(ws.RootDir, remoteName)
		if err != nil {
			return err
		}
		result, err := syncStore.SyncPull(ctx, remoteName, resolvedBranch)
		payload, handledErr := buildSyncPullPayload(remoteName, resolvedBranch, result.Message, err)
		if handledErr != nil {
			return handledErr
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			return printSyncPullPayload(w, v, *verbose)
		})
	case "push":
		fs := newCobraFlagSet("sync push")
		remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
		setUpstream := fs.Bool("set-upstream", false, "Pass -u to dolt push")
		force := fs.Bool("force", false, "Pass --force to dolt push")
		verbose := fs.Bool("verbose", false, "Include detailed remote output")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
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
				"raw":    "no upstream remote and no single configured remote; skipping sync push",
			}
			// [LAW:dataflow-not-control-flow] exception: explicit no-remote policy requires suppressing sync side effects when remote resolution yields empty input.
			return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
				return printSyncPushPayload(w, v, *verbose)
			})
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
			return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
				return printSyncPushPayload(w, v, *verbose)
			})
		}
		syncBranch, err := resolveSyncBranch(ws.RootDir, remoteName)
		if err != nil {
			return err
		}
		// [LAW:dataflow-not-control-flow] Sync push runs one deterministic embedded mutation path from resolved remote+branch state.
		result, err := syncStore.SyncPush(ctx, remoteName, syncBranch, *setUpstream, *force)
		traceMetadata := map[string]string{
			"remote":      remoteName,
			"sync_branch": syncBranch,
		}
		if strings.TrimSpace(result.Message) != "" {
			traceMetadata["message"] = strings.TrimSpace(result.Message)
		}
		traceStatus := "ok"
		traceReason := "managed automation requested sync push"
		if err != nil {
			traceStatus = "error"
			traceReason = err.Error()
			traceMetadata["error"] = err.Error()
		}
		syncCommandArgs := []string{"sync", "push", "--remote", remoteName}
		if *setUpstream {
			syncCommandArgs = append(syncCommandArgs, "--set-upstream")
		}
		if *force {
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
		if err != nil {
			return err
		}
		payload := map[string]any{
			"status":      "ok",
			"remote":      remoteName,
			"branch":      syncBranch,
			"raw":         result.Message,
			"push_status": result.Status,
		}
		if traceRef != nil {
			payload["trace_ref"] = traceRef.Path
		}
		if traceRecordErr != nil {
			payload["trace_error"] = traceRecordErr.Error()
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			return printSyncPushPayload(w, v, *verbose)
		})
	case "status":
		fs := newCobraFlagSet("sync status")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
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
		payload := map[string]any{
			"dolt_version": report.DoltVersion,
			"branch":       report.Branch,
			"head":         head,
			"head_commit":  report.HeadCommit,
			"head_message": report.HeadMessage,
			"status":       report.Status,
			"git_remotes":  syncState.gitRemotes,
			"dolt_remotes": syncState.doltRemotes,
			"changes":      syncState.changes,
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]any)
			_, err := fmt.Fprintf(
				w,
				"version=%v branch=%v head=%v git=%d dolt=%d added=%d updated=%d removed=%d\n",
				p["dolt_version"],
				p["branch"],
				p["head"],
				len(p["git_remotes"].([]workspace.GitRemote)),
				len(p["dolt_remotes"].([]store.SyncRemote)),
				len(syncState.changes.Added),
				len(syncState.changes.Updated),
				len(syncState.changes.Removed),
			)
			return err
		})
	default:
		return errors.New("usage: lit sync <status|remote|fetch|pull|push> ...")
	}
}

func firstNonEmptySyncBranch(candidates ...string) string {
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func resolveSyncRemote(requestedRemote string, upstreamRemote string, gitRemotes []workspace.GitRemote) (string, error) {
	validatedRequestedRemote := strings.TrimSpace(requestedRemote)
	if validatedRequestedRemote != "" {
		// [LAW:no-silent-fallbacks] Explicit remote that doesn't exist is a configuration error, not a skip condition.
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
	return firstNonEmptySyncRemote(validatedUpstreamRemote, singleRemote), nil
}

func firstNonEmptySyncRemote(candidates ...string) string {
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
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
	resolvedBranch := firstNonEmptySyncBranch(debugOverride, defaultBranch)
	if resolvedBranch == "" {
		return "", fmt.Errorf(
			"resolve sync branch for remote %q: default branch unavailable; configure %s to override",
			strings.TrimSpace(remote),
			debugSyncBranchEnvVar,
		)
	}
	return resolvedBranch, nil
}

func buildSyncPullPayload(remote string, requestedBranch string, output string, runErr error) (map[string]any, error) {
	if runErr == nil {
		return map[string]any{
			"status": "ok",
			"remote": remote,
			"branch": requestedBranch,
			"raw":    output,
		}, nil
	}
	message := strings.TrimSpace(runErr.Error())
	missingBranch, matchesMissingBranch := detectMissingRemoteBranch(message, requestedBranch)
	if !matchesMissingBranch {
		return nil, runErr
	}
	nextCommand := fmt.Sprintf("lit sync push --remote %s --set-upstream", remote)
	retryCommand := fmt.Sprintf("lit sync pull --remote %s", remote)
	// [LAW:dataflow-not-control-flow] Sync pull always returns structured payload; outcome variance lives in status/reason fields.
	return map[string]any{
		"status":        "skipped",
		"reason":        "remote_branch_missing",
		"remote":        remote,
		"branch":        missingBranch,
		"next_command":  nextCommand,
		"retry_command": retryCommand,
		"raw":           message,
	}, nil
}

func detectMissingRemoteBranch(message string, requestedBranch string) (string, bool) {
	// [LAW:single-enforcer] Remote-branch-missing classification is centralized here to avoid drift across callsites.
	normalized := strings.ToLower(strings.TrimSpace(message))
	if !strings.Contains(normalized, "not found on remote") {
		return "", false
	}
	matches := missingRemoteBranchPattern.FindStringSubmatch(message)
	branch := strings.TrimSpace(requestedBranch)
	if len(matches) == 2 && strings.TrimSpace(matches[1]) != "" {
		branch = strings.TrimSpace(matches[1])
	}
	if branch == "" {
		return "", false
	}
	return branch, true
}

func printSyncPullPayload(w io.Writer, v any, verbose bool) error {
	payload := v.(map[string]any)
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
	default:
		raw, hasRaw := payload["raw"].(string)
		if !verbose {
			_, err := fmt.Fprintln(w, "pulled")
			return err
		}
		if hasRaw && strings.TrimSpace(raw) != "" {
			_, err := fmt.Fprintln(w, raw)
			return err
		}
		if branch != "" {
			_, err := fmt.Fprintf(w, "pulled %s/%s\n", remote, branch)
			return err
		}
		_, err := fmt.Fprintf(w, "pulled %s\n", remote)
		return err
	}
}

func printSyncPushPayload(w io.Writer, v any, verbose bool) error {
	payload := v.(map[string]any)
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
		desiredURL := syncRemoteURL(remote.URL)
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
		desiredURL := syncRemoteURL(remote.URL)
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

func sameRemoteURL(left, right string) bool {
	return normalizeRemoteURL(left) == normalizeRemoteURL(right)
}

func syncRemoteURL(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "git+") {
		return trimmed
	}
	if !strings.HasSuffix(strings.ToLower(trimmed), ".git") {
		return trimmed
	}
	if strings.Contains(trimmed, "://") {
		// [LAW:one-source-of-truth] Git-backed Dolt remotes use one explicit `git+...` transport form instead of relying on procedure-side URL inference.
		return "git+" + trimmed
	}
	normalized := normalizeSCPLikeRemoteURL(trimmed)
	if normalized != trimmed {
		return "git+" + normalized
	}
	return trimmed
}

func normalizeRemoteURL(input string) string {
	trimmed := strings.TrimSpace(input)
	trimmed = strings.TrimPrefix(trimmed, "git+")
	if trimmed == "" {
		return ""
	}
	// [LAW:one-source-of-truth] Remote URL comparison uses one canonical normalizer so sync reconciliation decisions do not drift across URL spellings.
	trimmed = normalizeSCPLikeRemoteURL(trimmed)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return trimmed
	}
	parsed.Scheme = strings.ToLower(strings.TrimSpace(parsed.Scheme))
	parsed.Host = strings.ToLower(strings.TrimSpace(parsed.Host))
	parsed.Path = normalizeRemotePath(parsed.Path)
	return parsed.String()
}

func normalizeSCPLikeRemoteURL(input string) string {
	if strings.Contains(input, "://") {
		return input
	}
	separator := scpHostPathSeparator(input)
	if separator <= 0 {
		return input
	}
	hostPart := strings.TrimSpace(input[:separator])
	pathPart := strings.TrimSpace(input[separator+1:])
	if hostPart == "" || pathPart == "" || strings.Contains(hostPart, "/") {
		return input
	}
	if strings.HasPrefix(pathPart, "/") {
		return "ssh://" + hostPart + pathPart
	}
	return "ssh://" + hostPart + "/" + pathPart
}

func scpHostPathSeparator(input string) int {
	separator := -1
	inBrackets := false
	for index, character := range input {
		switch character {
		case '[':
			inBrackets = true
		case ']':
			inBrackets = false
		case ':':
			if !inBrackets {
				separator = index
				return separator
			}
		}
	}
	return separator
}

func normalizeRemotePath(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	cleaned := path.Clean(strings.TrimSpace(input))
	if strings.HasPrefix(input, "/") && !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return cleaned
}
