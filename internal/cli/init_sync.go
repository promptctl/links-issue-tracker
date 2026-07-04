package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// adoptRemoteTimeout caps the whole adopt. Adopt now CLONES (bulk whole-archive
// copy), which is fast — but the cap stays as defense-in-depth: dolt's git-backed
// transport does not honor context cancellation mid-operation, so a bounded,
// loud failure always beats any possibility of an unbounded hang on a setup step.
// (The prior fetch-based adopt routinely spent 20+ minutes re-inflating archive
// blobs for chunk-by-chunk reads; clone reads each blob once.)
// [LAW:no-ambient-temporal-coupling] init owns the time budget explicitly here;
// it does not depend on the remote, the network, or dolt finishing "eventually".
//
// A var, not a const, solely so tests can shorten it to force the deadline path
// deterministically; production never reassigns it.
var adoptRemoteTimeout = 120 * time.Second

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
// adoptRemoteTicketsBlockingFn is the adopt body, indirected through a var so a
// test can substitute a controllable stand-in and exercise the hard-stop
// without a real (slow, leak-prone) fetch. Production never reassigns it.
var adoptRemoteTicketsBlockingFn = adoptRemoteTicketsBlocking

func adoptRemoteTicketsOnInit(ctx context.Context, ws workspace.Info) initSyncOutcome {
	// dolt's fetch does NOT honor context cancellation mid-operation (verified: an
	// expired deadline does not unblock a slow fetch), so a context timeout alone
	// cannot stop a lockup. The adopt therefore runs in a goroutine we hard-stop
	// on a deadline: on expiry we abandon the in-flight work and report the
	// lockout loudly. The short-lived `lit init` process reclaims the abandoned
	// goroutine when it exits. A bounded, loud failure always beats an unbounded
	// hang. [LAW:no-ambient-temporal-coupling] [LAW:no-silent-failure]
	progressf("init", "checking whether a git remote already carries a lit backlog")
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan initSyncOutcome, 1)
	go func() { done <- adoptRemoteTicketsBlockingFn(runCtx, ws) }()
	select {
	case outcome := <-done:
		return outcome
	case <-time.After(adoptRemoteTimeout):
		return initSyncOutcome{
			State: initSyncFailed,
			Error: fmt.Sprintf(
				"adopting the remote backlog exceeded %s and was aborted — the store is empty, do NOT push "+
					"from it (a sync push of an empty store cannot help and risks the remote backlog). The data "+
					"transferred, but the embedded dolt pull did not finish processing it in time (a slow-adopt "+
					"issue in dolt's pull, not a transfer or data-size problem). Retry `lit init`; if it keeps "+
					"timing out, escalate the slow adopt rather than re-running blindly",
				adoptRemoteTimeout),
		}
	}
}

func adoptRemoteTicketsBlocking(ctx context.Context, ws workspace.Info) initSyncOutcome {
	plan, outcome := planRemoteAdopt(ctx, ws)
	if plan == nil {
		// A terminal outcome was already decided (local tickets to preserve, no
		// remote, remote empty, no lit data on the remote, or a resolution error).
		// planRemoteAdopt has closed its probe connection in every case.
		if situation := remoteSituationLine(outcome.State); situation != "" {
			progressf("init", "%s", situation)
		}
		return outcome
	}
	progressf("init", "remote %s/%s carries lit data (refs/dolt/*); downloading the backlog now", plan.remote, plan.branch)
	// The remote advertises lit data (refs/dolt/*) and the local store is empty,
	// so adopt it by CLONING — the bulk whole-archive transfer the git-backed
	// medium supports — rather than the chunk-by-chunk fetch pipeline that turned
	// a real backlog adopt into a 20-minute lockup. The probe connection is
	// closed; AdoptRemoteByClone takes the exclusive workspace hold and swaps the
	// Dolt directory in place. [LAW:decomposition] adopt is a clone, not a fetch.
	if err := store.AdoptRemoteByClone(ctx, ws.DatabasePath, ws.WorkspaceID, plan.remote, plan.url, plan.branch); err != nil {
		// The remote DID carry data (we checked before cloning), so an empty
		// store here is a hazard, not a benign result: surface it loudly and name
		// the refs/dolt/* data we could not adopt so the operator knows not to
		// push from the empty store. [LAW:no-silent-failure]
		return initSyncOutcome{
			State:  initSyncFailed,
			Remote: plan.remote,
			Branch: plan.branch,
			Error: fmt.Sprintf(
				"remote %q carries lit ticket data (refs/dolt/*) but cloning it into the local store failed, so the store "+
					"is empty — do NOT push from it (a sync push of an empty store cannot help and risks the remote "+
					"backlog). Retry `lit init`; underlying error: %v",
				plan.remote, err,
			),
		}
	}
	return initSyncOutcome{State: initSyncAdopted, Remote: plan.remote, Branch: plan.branch}
}

// adoptClonePlan carries the resolved inputs the clone-based adopt needs. It is
// produced only when the remote genuinely carries lit data and the local store
// is empty — i.e. only when an adopt should actually happen.
type adoptClonePlan struct {
	remote string
	branch string
	url    string
}

// planRemoteAdopt opens a short-lived probe connection to decide whether init
// should adopt a remote backlog, returning either a non-nil plan (clone this)
// or a terminal initSyncOutcome (do nothing more). It always closes its probe
// connection before returning, so the caller can take the exclusive workspace
// hold the clone-swap requires. The "remote carries lit data" decision is made
// from the authoritative refs/dolt/* signal directly — not derived from a
// post-fetch tracking ref — so no slow fetch happens here.
// [LAW:dataflow-not-control-flow] [LAW:types-are-the-program]
func planRemoteAdopt(ctx context.Context, ws workspace.Info) (*adoptClonePlan, initSyncOutcome) {
	// Preserve any existing local backlog: only an empty or absent store may
	// adopt, because adopt-by-clone replaces the store wholesale. The check does
	// not create the store, so a fresh init leaves the target path untouched for
	// the clone to be its first writer. [LAW:no-silent-failure]
	hasTickets, err := store.LocalHasTickets(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return nil, initSyncOutcome{State: initSyncFailed, Error: err.Error()}
	}
	if hasTickets {
		return nil, initSyncOutcome{State: initSyncHasLocalTickets}
	}

	// The whole adopt decision is made from git signals alone — no Dolt store is
	// opened — so a fresh store's first on-disk state is the clone itself.
	gitRemotes, err := workspace.GitRemotes(ws.RootDir)
	if err != nil {
		return nil, initSyncOutcome{State: initSyncFailed, Error: err.Error()}
	}
	remote, err := resolveSyncRemote("", workspace.UpstreamRemote(ws.RootDir), gitRemotes)
	if err != nil {
		return nil, initSyncOutcome{State: initSyncFailed, Error: err.Error()}
	}
	if remote == "" {
		return nil, initSyncOutcome{State: initSyncNotConfigured}
	}
	// [LAW:single-enforcer] First-push detection is centralized so init, pull,
	// and push share one definition of "remote is empty".
	hasRefs, refsErr := workspace.RemoteHasRefs(ws.RootDir, remote)
	if refsErr != nil {
		return nil, initSyncOutcome{State: initSyncFailed, Remote: remote, Error: refsErr.Error()}
	}
	if !hasRefs {
		return nil, initSyncOutcome{State: initSyncRemoteEmpty, Remote: remote}
	}
	branch, err := resolveSyncBranch(ws.RootDir, remote)
	if err != nil {
		return nil, initSyncOutcome{State: initSyncFailed, Remote: remote, Error: err.Error()}
	}
	// The refs/dolt/* namespace is the authoritative "remote carries lit data"
	// signal; RemoteHasRefs (any git ref) cannot tell a code-only repo from one
	// carrying a backlog. A code-only remote (refs but no lit data) is a genuine
	// empty result, reported silently; a remote that DOES carry data is adopted
	// by clone. The old silent-empty-despite-data bug is now unrepresentable:
	// hasData==true always leads to a clone, never to a silent empty store.
	// [LAW:no-silent-failure] [LAW:types-are-the-program]
	hasData, dataErr := workspace.RemoteHasDoltData(ws.RootDir, remote)
	if dataErr != nil {
		return nil, initSyncOutcome{State: initSyncFailed, Remote: remote, Branch: branch, Error: dataErr.Error()}
	}
	if !hasData {
		return nil, initSyncOutcome{State: initSyncNoRemoteData, Remote: remote, Branch: branch}
	}
	url := gitBackedURLForRemote(gitRemotes, remote)
	if url == "" {
		return nil, initSyncOutcome{
			State:  initSyncFailed,
			Remote: remote,
			Branch: branch,
			Error:  fmt.Sprintf("remote %q carries lit data but its git URL could not be resolved for clone", remote),
		}
	}
	return &adoptClonePlan{remote: remote, branch: branch, url: url}, initSyncOutcome{}
}

// remoteSituationLine renders the resolved remote-data situation for the
// benign non-adopt outcomes, so a user watching init knows what lit decided
// about the remote rather than inferring it from silence. The failed state is
// deliberately absent: a failure has exactly one loud channel
// (writeInitSyncLine's warning), never a second copy here. [LAW:single-enforcer]
// An exhaustive match on the sealed state — variability lives in the outcome
// value, not in whether a caller logs. [LAW:dataflow-not-control-flow]
func remoteSituationLine(state initSyncState) string {
	switch state {
	case initSyncHasLocalTickets:
		return "local store already holds tickets; leaving it untouched (ongoing sync handles updates)"
	case initSyncNotConfigured:
		return "no eligible git remote; starting with an empty backlog"
	case initSyncRemoteEmpty:
		return "remote has no refs yet (brand-new repo); starting with an empty backlog"
	case initSyncNoRemoteData:
		return "remote has git refs but no lit data; starting with an empty backlog"
	default:
		return ""
	}
}

// gitBackedURLForRemote returns the Dolt git-backed transport URL for the named
// git remote, or "" when no such remote is present. [LAW:one-source-of-truth]
// the translation is store.GitBackedRemoteURL, the same one sync uses.
func gitBackedURLForRemote(gitRemotes []workspace.GitRemote, remote string) string {
	for _, r := range gitRemotes {
		if r.Name == remote {
			return store.GitBackedRemoteURL(r.URL)
		}
	}
	return ""
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
