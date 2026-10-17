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
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// claimContext bundles everything a claim-aware render needs, gathered once
// per command so every row's annotation reads from values instead of
// re-querying the store or the filesystem per row.
//
// addresses is the local half of "Finding a claimant"
// (design-docs/work-claims.md): it resolves a claimant's attribution to a live
// worktree's path and branch only when this machine's own liveness enumeration
// proves one exists. It is built straight from workspace.LiveCheckouts rather
// than through app.LocalCheckouts (which projects the same enumeration down to
// bare tokens for the claim predicate) because rendering needs the path and
// branch that projection throws away — the address book already exists; render
// it, don't re-enumerate. Nothing in this map ever reaches the shared database:
// it lives only as long as this command's process. [privacy invariant]
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
	evidence, err := readEvidence(ctx, ap)
	if err != nil {
		return claimContext{}, err
	}

	// [LAW:no-silent-failure] This machine cannot prove which of its own
	// worktrees are still alive. Fall back to the zero LocalCheckouts,
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
		local = claims.NewLocalCheckouts(ap.Workspace.WorkspaceID, app.LiveCheckoutsOf(checkouts))
		addresses = addressesByAttribution(ap.Workspace.WorkspaceID, checkouts)
	}

	fresh := claims.Freshness{Now: time.Now(), Window: cfg.Claims.FreshnessWindow}
	standings := claims.Derive(evidence, fresh, local)
	return claimContext{standings: standings, evidence: evidence, self: ownAttribution(ap), addresses: addresses}, nil
}

// readEvidence reads the claim evidence out of the store: every issue, the
// parent each sits under, and every event.
func readEvidence(ctx context.Context, ap *app.App) (claims.Evidence, error) {
	// IncludeArchived/IncludeDeleted: evidence.go is explicit that claim
	// derivation needs every issue an event could name, closed ones
	// included, because a checkout's hold on a lane can rest entirely on a
	// `done` against a ticket no longer open — and a deleted or archived
	// issue is exactly such a ticket, still named by its own historical
	// events. The zero-value filter excludes both, so with it a repository
	// holding even one deleted issue that ever carried an event would make
	// NewEvidence fail outright on every `next` and `backlog` invocation.
	allIssues, err := ap.Store.ListIssues(ctx, storage.ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true})
	if err != nil {
		return claims.Evidence{}, err
	}
	ids := make([]string, len(allIssues))
	for i, issue := range allIssues {
		ids[i] = issue.ID
	}
	relations, err := ap.Store.GetRelationsByIDs(ctx, ids)
	if err != nil {
		return claims.Evidence{}, err
	}
	parents := make(map[string]*model.Issue, len(allIssues))
	for _, issue := range allIssues {
		parents[issue.ID] = relations[issue.ID].Parent
	}
	events, err := ap.Store.ListAllEvents(ctx)
	if err != nil {
		return claims.Evidence{}, err
	}
	return claims.NewEvidence(allIssues, parents, events)
}

// ownAttribution is the attribution this checkout's own work carries — the
// pair every claim comparison in this package tests against, spelled once.
// [LAW:one-source-of-truth]
//
// NewAttribution collapses an absent stream (a checkout that has never mutated)
// to the zero Attribution, the public checkout's. No branch is needed for that
// case: relationOf never counts a zero self as holding anything.
// [LAW:dataflow-not-control-flow]
func ownAttribution(ap *app.App) model.Attribution {
	return model.NewAttribution(ap.Stream.Value(), ap.Workspace.WorkspaceID)
}

// transferNotice is the line a start owes when it takes the ticket FROM
// somebody, and the empty string when it owes none — the identity value for
// "nothing to say", so the caller writes the result unconditionally instead of
// guarding the write. [LAW:dataflow-not-control-flow]
//
// The handover is the one Apply reports, read off the comparison that decided
// whether the start owed a write, so the line names exactly the transfer the
// history records. [LAW:one-source-of-truth] laneHeld is the takeover gate's
// reading of the lane: a lane nobody holds has no claim to hand over — it was
// never started, is finished, or its claim expired — and the notice would be
// wrong whatever the row's own history records about who last started it,
// because an expired claim is not a claim.
func transferNotice(laneHeld bool, handover *claims.Handover) string {
	if !laneHeld || handover == nil {
		return ""
	}
	return fmt.Sprintf("claim transferred: %s -> %s\n", describeClaimant(handover.From), describeClaimant(handover.To))
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
