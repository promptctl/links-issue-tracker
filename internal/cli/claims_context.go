package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// claimContext bundles everything a claim-aware render needs, gathered once
// per command so every row's annotation reads from values instead of
// re-querying the store or the filesystem per row.
//
// addresses is the local half of "Finding a claimant"
// (design-docs/work-claims.md): it resolves a claimant's attribution to a
// live worktree's path and branch only when this machine's own liveness
// enumeration proves one exists. It is built straight from
// workspace.LiveCheckouts rather than through app.LocalCheckouts (which
// projects the same enumeration down to bare tokens for the claim
// predicate) because rendering needs the path and branch that projection
// throws away — per the ticket's own review comment, the address book
// already exists; render it, don't re-enumerate. Nothing in this map ever
// reaches the shared database: it lives only as long as this command's
// process. [privacy invariant]
type claimContext struct {
	standings claims.Standings
	evidence  claims.Evidence
	self      model.Attribution
	addresses map[model.Attribution]workspace.Checkout
}

// gatherClaimContext is the one place `next` and `backlog` each read claim
// evidence and local liveness — the store/filesystem boundary claim-aware
// rendering and routing both sit behind. [LAW:effects-at-boundaries]
func gatherClaimContext(ctx context.Context, stdout io.Writer, ap *app.App) (claimContext, error) {
	cfg, err := config.Load(pathspec.New(ap.Workspace.RootDir))
	if err != nil {
		return claimContext{}, err
	}
	// IncludeArchived/IncludeDeleted: evidence.go is explicit that claim
	// derivation needs every issue an event could name, closed ones
	// included, because a checkout's hold on a lane can rest entirely on a
	// `done` against a ticket no longer open — and a deleted or archived
	// issue is exactly such a ticket, still named by its own historical
	// events. The zero-value filter excludes both, which is why a
	// repository with even one deleted issue that ever carried an event
	// (this one included) made NewEvidence fail outright on every `next` and
	// `backlog` invocation before this widened the read.
	allIssues, err := ap.Store.ListIssues(ctx, store.ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true})
	if err != nil {
		return claimContext{}, err
	}
	ids := make([]string, len(allIssues))
	for i, issue := range allIssues {
		ids[i] = issue.ID
	}
	relations, err := ap.Store.GetRelationsByIDs(ctx, ids)
	if err != nil {
		return claimContext{}, err
	}
	parents := make(map[string]*model.Issue, len(allIssues))
	for _, issue := range allIssues {
		parents[issue.ID] = relations[issue.ID].Parent
	}
	events, err := ap.Store.ListAllEvents(ctx)
	if err != nil {
		return claimContext{}, err
	}
	evidence, err := claims.NewEvidence(allIssues, parents, events)
	if err != nil {
		return claimContext{}, err
	}

	// [LAW:no-silent-failure] This machine cannot prove which of its own
	// worktrees are still alive. The judgment call left open by
	// links-claims-1ihf.4's comment: fall back to the zero LocalCheckouts,
	// which voids nothing and lets freshness alone govern — but say so,
	// every time, because the fallback silently changes which lanes route
	// around this checkout otherwise, and it means no address ever resolves
	// this run.
	var local claims.LocalCheckouts
	var addresses map[model.Attribution]workspace.Checkout
	checkouts, err := workspace.LiveCheckouts(ap.Workspace.RootDir)
	if err != nil {
		if _, printErr := fmt.Fprintf(stdout, "warning: could not enumerate local checkouts (%v) — claim liveness check and local addresses skipped, freshness alone governs\n", err); printErr != nil {
			return claimContext{}, printErr
		}
	} else {
		local = claims.NewLocalCheckouts(ap.Workspace.WorkspaceID, checkoutStreamTokens(checkouts))
		addresses = addressesByAttribution(ap.Workspace.WorkspaceID, checkouts)
	}

	fresh := claims.Freshness{Now: time.Now(), Window: cfg.Claims.FreshnessWindow}
	standings := claims.Derive(evidence, fresh, local)
	// NewAttribution collapses an absent stream (a checkout that has never
	// mutated) to the zero Attribution, which is exactly "no live claims" —
	// no branch needed here for the never-minted case.
	self := model.NewAttribution(ap.Stream.Value(), ap.Workspace.WorkspaceID)
	return claimContext{standings: standings, evidence: evidence, self: self, addresses: addresses}, nil
}

// checkoutStreamTokens projects enumerated checkouts onto the tokens claim
// derivation's liveness leg compares evidence against. Mirrors
// app.streamTokens: a checkout that has never mutated carries no token and
// contributes none, since it holds no claim either way.
func checkoutStreamTokens(checkouts []workspace.Checkout) []string {
	tokens := make([]string, 0, len(checkouts))
	for _, checkout := range checkouts {
		if checkout.Stream.Present() {
			tokens = append(tokens, checkout.Stream.Value())
		}
	}
	return tokens
}

// addressesByAttribution indexes this machine's live checkouts by the
// attribution their claims would carry, so rendering can look a holder up by
// the same pair the shared evidence already carries. A checkout with no
// stream token has made no claim and is correctly absent.
func addressesByAttribution(workspaceID string, checkouts []workspace.Checkout) map[model.Attribution]workspace.Checkout {
	addresses := make(map[model.Attribution]workspace.Checkout, len(checkouts))
	for _, checkout := range checkouts {
		if checkout.Stream.Present() {
			addresses[model.NewAttribution(checkout.Stream.Value(), workspaceID)] = checkout
		}
	}
	return addresses
}
