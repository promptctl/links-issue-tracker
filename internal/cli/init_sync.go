package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// initSyncState classifies what init's remote-adopt step did. It is the single
// discriminant the renderer switches on, so a label that contradicts the outcome
// is unrepresentable; the loose strings never appear at a callsite.
// [LAW:types-are-the-program]
type initSyncState string

const (
	// initSyncHasLocalTickets: the store already holds local tickets, so adopt
	// would risk losing them and is not attempted — ongoing sync handles updates.
	initSyncHasLocalTickets initSyncState = "has_local_tickets"
	// initSyncNotConfigured: no eligible git remote to adopt from.
	initSyncNotConfigured initSyncState = "not_configured"
	// initSyncRemoteEmpty: the remote advertises no refs yet (brand-new repo).
	initSyncRemoteEmpty initSyncState = "remote_empty"
	// initSyncNoRemoteData: the remote has git refs but no lit data on the branch.
	initSyncNoRemoteData initSyncState = "no_remote_data"
	// initSyncAdopted: the existing backlog was pulled into the fresh store.
	initSyncAdopted initSyncState = "adopted"
	// initSyncFailed: adopt was attempted but errored; the workspace is still
	// initialized (empty), and the error is surfaced — never swallowed.
	// [LAW:no-silent-failure]
	initSyncFailed initSyncState = "failed"
)

// initSyncOutcome is the result of init's detect-and-adopt step, independent of
// presentation. Remote/Branch are set once they are resolved; Error is set only
// for initSyncFailed.
type initSyncOutcome struct {
	State  initSyncState `json:"state"`
	Remote string        `json:"remote,omitempty"`
	Branch string        `json:"branch,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// adoptRemoteTicketsOnInit detects whether the configured git remote already
// carries lit/Dolt ticket data and, when the local store has no tickets to lose,
// adopts that history wholesale so a fresh clone transparently picks up the
// existing backlog. The authoritative store lives in .git/links/dolt and does
// not ride the git working tree, so init is the one place that can make
// "clone + init = my tickets are here" true.
//
// The gate is local emptiness, not "created this run": a store with zero issues
// has no work to lose, which makes the destructive reset safe and also lets a
// re-init after a transient failure retry the adopt. A plain pull cannot do this
// — a freshly-initialized store's bootstrap root is unrelated to the remote, so
// a merge fails with "no common ancestor"; adopt resets to the remote head.
//
// It is best-effort within init: a failure to reach or read the remote is
// surfaced loudly in the outcome but does not refuse init, because the workspace
// itself is validly initialized (empty). [LAW:no-silent-failure]
func adoptRemoteTicketsOnInit(ctx context.Context, ws workspace.Info) initSyncOutcome {
	syncStore, err := store.OpenSync(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return initSyncOutcome{State: initSyncFailed, Error: err.Error()}
	}
	defer syncStore.Close()

	localIssues, err := syncStore.LocalIssueCount(ctx)
	if err != nil {
		return initSyncOutcome{State: initSyncFailed, Error: err.Error()}
	}
	if localIssues > 0 {
		return initSyncOutcome{State: initSyncHasLocalTickets}
	}

	syncState, err := syncDoltRemotesFromGit(ctx, syncStore, ws)
	if err != nil {
		return initSyncOutcome{State: initSyncFailed, Error: err.Error()}
	}
	remote, err := resolveSyncRemote("", workspace.UpstreamRemote(ws.RootDir), syncState.gitRemotes)
	if err != nil {
		return initSyncOutcome{State: initSyncFailed, Error: err.Error()}
	}
	if remote == "" {
		return initSyncOutcome{State: initSyncNotConfigured}
	}
	// [LAW:single-enforcer] First-push detection is centralized so init, pull,
	// and push share one definition of "remote is empty".
	hasRefs, refsErr := workspace.RemoteHasRefs(ws.RootDir, remote)
	if refsErr != nil {
		return initSyncOutcome{State: initSyncFailed, Remote: remote, Error: refsErr.Error()}
	}
	if !hasRefs {
		return initSyncOutcome{State: initSyncRemoteEmpty, Remote: remote}
	}
	branch, err := resolveSyncBranch(ws.RootDir, remote)
	if err != nil {
		return initSyncOutcome{State: initSyncFailed, Remote: remote, Error: err.Error()}
	}
	// Fetch populates remotes/<remote>/<branch>; SyncFreshness.Synced then reports
	// whether the remote actually carries lit data on that branch — the adopt
	// signal links-doctor-9dnu's freshness check was built to answer.
	if err := syncStore.SyncFetch(ctx, remote, false); err != nil {
		return initSyncOutcome{State: initSyncFailed, Remote: remote, Branch: branch, Error: err.Error()}
	}
	freshness, err := syncStore.SyncFreshness(ctx, remote, branch)
	if err != nil {
		return initSyncOutcome{State: initSyncFailed, Remote: remote, Branch: branch, Error: err.Error()}
	}
	if !freshness.Synced {
		return initSyncOutcome{State: initSyncNoRemoteData, Remote: remote, Branch: branch}
	}
	if err := syncStore.SyncResetToRemoteHead(ctx, remote, branch); err != nil {
		return initSyncOutcome{State: initSyncFailed, Remote: remote, Branch: branch, Error: err.Error()}
	}
	return initSyncOutcome{State: initSyncAdopted, Remote: remote, Branch: branch}
}

// writeInitSyncLine renders the one human-facing line the adopt step contributes.
// Adopted and failed are the states a user must see; the other outcomes mean
// "fresh empty workspace was the right result", already conveyed by the headline.
func writeInitSyncLine(w io.Writer, outcome initSyncOutcome) error {
	switch outcome.State {
	case initSyncAdopted:
		_, err := fmt.Fprintf(w, "  Pulled existing backlog from %s/%s\n", outcome.Remote, outcome.Branch)
		return err
	case initSyncFailed:
		_, err := fmt.Fprintf(
			w,
			"  Warning: could not pull existing backlog%s: %s\n",
			remoteSuffix(outcome.Remote),
			outcome.Error,
		)
		return err
	default:
		return nil
	}
}

func remoteSuffix(remote string) string {
	if remote == "" {
		return ""
	}
	return " from " + remote
}
