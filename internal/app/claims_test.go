package app

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// gitRepoWithCommit is gitRepo plus the commit `git worktree add` needs to
// branch from.
func gitRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := gitRepo(t)
	run(t, dir, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com",
		"commit", "--allow-empty", "-m", "init")
	return dir
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
}

// openIn opens one checkout's app, runs body against it, and closes it before
// returning. The store is embedded Dolt and admits one read-write engine per
// path, so two worktrees of one repository must take turns rather than hold
// overlapping opens — which is also how the real commands use it.
func openIn(t *testing.T, checkout string, mode AccessMode, body func(*App)) {
	t.Helper()
	ap, err := Open(context.Background(), checkout, mode)
	if err != nil {
		t.Fatalf("Open(%q, %v) error = %v", checkout, mode, err)
	}
	defer func() {
		if err := ap.Close(); err != nil {
			t.Fatalf("Close(%q) error = %v", checkout, err)
		}
	}()
	body(ap)
}

// readEvidence walks the read path a claims consumer walks: every issue
// including the ones that have left the flow, their parents, and the whole
// history. The issue list is deliberately unfiltered — a lane's establishing
// event can sit on a ticket that is already closed.
func readEvidence(t *testing.T, ap *App) claims.Evidence {
	t.Helper()
	ctx := context.Background()
	issues, err := ap.Store.ListIssues(ctx, store.ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	relations, err := ap.Store.GetRelationsByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("GetRelationsByIDs() error = %v", err)
	}
	parents := make(map[string]*model.Issue, len(relations))
	for id, rel := range relations {
		parents[id] = rel.Parent
	}
	events, err := ap.Store.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents() error = %v", err)
	}
	evidence, err := claims.NewEvidence(issues, parents, events)
	if err != nil {
		t.Fatalf("NewEvidence() error = %v", err)
	}
	return evidence
}

func fresh() claims.Freshness {
	return claims.Freshness{Now: time.Now(), Window: 24 * time.Hour}
}

// TestDeletedCheckoutReleasesItsClaimHereAndAgesOutElsewhere is the ticket's
// acceptance, run literally against real worktrees and the real store.
//
// A second checkout takes a lane. This machine reads it as held. The worktree is
// then deleted, and the very next derivation on this machine — no waiting, no
// window to lapse, no cleanup step in between — finds the lane free. A second
// clone, which carries a different workspace id and cannot see this filesystem,
// derives the same evidence and still finds the lane held; it waits out
// freshness like any remote. The asymmetry is the design's, and it is honest:
// deletion is a local fact, and only its owner can observe it instantly.
func TestDeletedCheckoutReleasesItsClaimHereAndAgesOutElsewhere(t *testing.T) {
	primary := gitRepoWithCommit(t)
	worker := filepath.Join(t.TempDir(), "worker")
	run(t, primary, "git", "worktree", "add", worker)

	var lane model.LaneID
	var workerToken string
	openIn(t, worker, AccessWrite, func(ap *App) {
		workerToken = ap.Stream.Value()
		if workerToken == "" {
			t.Fatal("a write open must mint the checkout's identity")
		}
		issue, err := ap.Store.CreateIssue(context.Background(), store.CreateIssueInput{
			Prefix: "test", Title: "Work the worker took", Topic: "claims", IssueType: "task",
		})
		if err != nil {
			t.Fatalf("CreateIssue() error = %v", err)
		}
		if _, err := ap.Store.Apply(context.Background(), issue.ID, store.Change{
			Action: model.Start{Assignee: "worker"}, Actor: "worker", Reason: "begin",
		}); err != nil {
			t.Fatalf("Apply(start) error = %v", err)
		}
		lane = model.LaneOf(issue, nil)
	})

	var evidence claims.Evidence
	var workspaceID string
	openIn(t, primary, AccessRead, func(ap *App) {
		workspaceID = ap.Workspace.WorkspaceID
		evidence = readEvidence(t, ap)
		local, err := ap.LocalCheckouts()
		if err != nil {
			t.Fatalf("LocalCheckouts() error = %v", err)
		}
		standing := claims.Derive(evidence, fresh(), local).Of(lane)
		held, ok := standing.(claims.Held)
		if !ok {
			t.Fatalf("standing while the worker's checkout is alive = %#v, want Held", standing)
		}
		if held.By.Stream() != workerToken {
			t.Fatalf("holder stream = %q, want the worker's %q", held.By.Stream(), workerToken)
		}
	})

	run(t, primary, "git", "worktree", "remove", "--force", worker)

	openIn(t, primary, AccessRead, func(ap *App) {
		local, err := ap.LocalCheckouts()
		if err != nil {
			t.Fatalf("LocalCheckouts() after the removal error = %v", err)
		}
		// Read fresh rather than reusing the value above: the same evidence must
		// derive differently, so re-reading it would leave open whether the
		// change came from the enumeration or from the database moving.
		standing := claims.Derive(readEvidence(t, ap), fresh(), local).Of(lane)
		if _, free := standing.(claims.Unclaimed); !free {
			t.Fatalf("standing after the worker's worktree was deleted = %#v, want the lane free on this machine's very next selection", standing)
		}
	})

	// The second clone: a different workspace id, and checkouts of its own that
	// have nothing to say about this workspace's evidence.
	elsewhere := claims.NewLocalCheckouts(workspaceID+"-a-different-clone", []string{"othr23456defgh"})
	remote := claims.Derive(evidence, fresh(), elsewhere).Of(lane)
	held, ok := remote.(claims.Held)
	if !ok {
		t.Fatalf("standing on a second clone = %#v, want Held: it cannot see this filesystem and must age the claim out instead", remote)
	}
	if held.By.Stream() != workerToken {
		t.Fatalf("second clone's holder stream = %q, want the worker's %q", held.By.Stream(), workerToken)
	}
}

// TestStreamTokensCountsOnlyMintedIdentities states the projection's contract
// on its own, because the enumeration's live checkouts and the live TOKENS are
// different sets and the difference is exactly the never-mutated checkout. The
// zero StreamID is the only one constructible from outside its package, which is
// the case that matters here.
func TestStreamTokensCountsOnlyMintedIdentities(t *testing.T) {
	got := streamTokens([]workspace.Checkout{
		{Path: "/never-mutated", Branch: "main"},
		{Path: "/also-never-mutated"},
	})
	if len(got) != 0 {
		t.Fatalf("streamTokens() = %q, want none: a checkout that has minted no identity contributes no token", got)
	}
}

// TestLocalCheckoutsScopesTokensToThisWorkspace pins the pairing this boundary
// exists to make. The tokens are useless without the workspace id that scopes
// them: unscoped, this machine's enumeration would prune a different clone's
// claims, which is a correctness bug and a privacy one at once — one store
// reaching conclusions about a store it was never given.
func TestLocalCheckoutsScopesTokensToThisWorkspace(t *testing.T) {
	primary := gitRepoWithCommit(t)
	openIn(t, primary, AccessWrite, func(ap *App) {
		local, err := ap.LocalCheckouts()
		if err != nil {
			t.Fatalf("LocalCheckouts() error = %v", err)
		}
		mine := model.NewAttribution("nosuchtoken1", ap.Workspace.WorkspaceID)
		if !local.Void(mine) {
			t.Fatal("a token of this workspace that belongs to no live checkout must be void")
		}
		foreign := model.NewAttribution("nosuchtoken1", ap.Workspace.WorkspaceID+"-elsewhere")
		if local.Void(foreign) {
			t.Fatal("another clone's evidence must never be pruned by this store's enumeration")
		}
		ours := model.NewAttribution(ap.Stream.Value(), ap.Workspace.WorkspaceID)
		if local.Void(ours) {
			t.Fatal("this very checkout was enumerated as dead")
		}
	})
}
