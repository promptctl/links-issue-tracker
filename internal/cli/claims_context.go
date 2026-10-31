package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/model"
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
func gatherClaimContext(ctx context.Context, env environ.Env, stdout io.Writer, ap *app.App) (claimContext, error) {
	evidence, err := readEvidence(ctx, ap)
	if err != nil {
		return claimContext{}, err
	}
	checkouts, err := gatherCheckouts(env, stdout, ap)
	if err != nil {
		return claimContext{}, err
	}
	standings := claims.Derive(evidence, checkouts.fresh, checkouts.local)
	return claimContext{standings: standings, evidence: evidence, self: ownAttribution(ap), addresses: checkouts.addresses}, nil
}

// checkoutFacts is what a derivation reads besides the evidence, and what a
// claim line needs to name a holder this machine can address: the clock against
// the configured window, and this machine's own checkouts.
type checkoutFacts struct {
	fresh     claims.Freshness
	local     claims.LocalCheckouts
	addresses map[model.Attribution]workspace.Checkout
}

// gatherCheckouts reads those facts, the half of a claim's reading that a read
// of one lane and a read of the backlog share.
func gatherCheckouts(env environ.Env, stdout io.Writer, ap *app.App) (checkoutFacts, error) {
	cfg, err := config.Load(env, ap.Workspace.Project)
	if err != nil {
		return checkoutFacts{}, err
	}

	// [LAW:no-silent-failure] This machine cannot prove which of its own
	// worktrees are still alive. Fall back to the zero LocalCheckouts,
	// which voids nothing and lets freshness alone govern — but say so,
	// every time, because the fallback silently changes which lanes route
	// around this checkout otherwise, and it means no address ever resolves
	// this run.
	var local claims.LocalCheckouts
	var addresses map[model.Attribution]workspace.Checkout
	checkouts, err := workspace.LiveCheckouts(env, ap.Workspace)
	if err != nil {
		if _, printErr := fmt.Fprintf(stdout, "warning: could not enumerate local checkouts (%v) — claim liveness check and local addresses skipped, freshness alone governs\n", err); printErr != nil {
			return checkoutFacts{}, printErr
		}
	} else {
		local = claims.NewLocalCheckouts(ap.Workspace.WorkspaceID, app.LiveCheckoutsOf(checkouts))
		addresses = addressesByAttribution(ap.Workspace.WorkspaceID, checkouts)
	}

	fresh := claims.Freshness{Now: time.Now(), Window: cfg.Claims.FreshnessWindow}
	return checkoutFacts{fresh: fresh, local: local, addresses: addresses}, nil
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
	// Every parent is itself an issue, so it is already in allIssues: the edges
	// are all this needs from the store, not a second hydrated copy of every
	// issue. [LAW:one-source-of-truth]
	parentByChild, err := ap.Store.ParentByChild(ctx)
	if err != nil {
		return claims.Evidence{}, err
	}
	events, err := ap.Store.ListAllEvents(ctx)
	if err != nil {
		return claims.Evidence{}, err
	}
	return claims.NewEvidence(allIssues, parentByChild, events)
}

// ownAttribution is the attribution this checkout's own work carries — the
// pair every claim comparison in this package tests against, spelled once.
// Store.AttributeTo derives the pair the store stamps on its events from the
// same stream token, which is what makes the takeover gate's "ours" the
// checkout the start is recorded under. [LAW:one-source-of-truth]
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
// history records. [LAW:one-source-of-truth] It describes the ticket's own
// record, though, and a claim is held per lane: lane is the standing the
// takeover gate read, and the line is owed only when that standing names the
// handover's predecessor as the holder (Handover.Live). Otherwise the
// predecessor held nothing to hand over, because an expired claim is not a
// claim.
func transferNotice(lane claims.Standing, handover *claims.Handover) string {
	if handover == nil || !handover.Live(lane) {
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
