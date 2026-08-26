package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
)

// `lit next` used to be one more workableView preset over the shared
// backlog/next pipeline (see workable.go): order, keep the first ready row,
// render. Claim routing broke that shape — the pick is no longer "the first
// ready row of an ordered list," it is a multi-step precedence over data
// (claim standings, this checkout's identity) the backlog view never reads,
// producing a discriminated NextOutcome the old single-row keep()/render()
// signature has no way to carry. Forking next out of workableView is the
// honest move once the shapes diverge (backlog stays exactly what it was);
// stretching the shared preset to fit would have re-tangled the two.
// [LAW:decomposition] [LAW:carrying-cost]
const nextUsage = "usage: lit next [--type ...] [--status ...] [--labels ...] [--continue] [--assignee <user>]"

func runNext(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("next")
	assignee := fs.String("assignee", "", "Filter by assignee")
	issueType := fs.String("type", "", "Filter by issue type")
	status := fs.String("status", "", "Filter by status: open|in_progress")
	labels := fs.String("labels", "", "Comma-separated labels all of which must match")
	// --continue predates claim routing and is retired by links-claims-1ihf.10,
	// which depends on this ticket. Left wired here unchanged: claim routing
	// already subsumes it for a checkout with live claims (routing step 2 IS
	// the epic-affinity bias, made unconditional and correct), and this flag
	// still matters as a plain rank-order tiebreak for a checkout with none.
	continueBias := fs.Bool("continue", false, "Bias toward leaves under in-progress epics")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: nextUsage}
	}
	statusState, err := parseWorkableStatus(*status)
	if err != nil {
		return err
	}
	issueTypeValue, err := parseWorkableType(*issueType)
	if err != nil {
		return err
	}
	// [LAW:single-enforcer] Same staleness warning, same position, as every
	// other ordinary read command (links-sync-pgct.2).
	if err := printSyncStalenessWarning(ctx, stdout, ap.Workspace, ap.Store, time.Now()); err != nil {
		return err
	}
	rows, details, err := gatherWorkableAnnotated(ctx, ap, workableFilter{
		Assignee:  strings.TrimSpace(*assignee),
		IssueType: issueTypeValue,
		Status:    statusState,
		Labels:    splitCSV(*labels),
	})
	if err != nil {
		return err
	}
	if *continueBias {
		sortByContinueBias(rows, details)
	}
	standings, self, err := claimStandings(ctx, stdout, ap)
	if err != nil {
		return err
	}
	occasion, err := renderNextOutcome(stdout, routeNext(rows, details, standings, self))
	if err != nil {
		return err
	}
	return workflows.Dispatch(stdout, os.Stderr, ap.Workspace, occasion)
}

// claimStandings derives every lane's claim standing and this checkout's own
// attribution — the two facts routeNext needs from the claims package, and
// the only store/filesystem reads `next` performs beyond the shared workable
// gather. Gathered once here and handed down as values, so routeNext itself
// stays pure. [LAW:effects-at-boundaries]
//
// Evidence needs every issue and event, closed ones included — a checkout's
// hold on a lane can rest entirely on a `done` against a ticket no longer
// open (internal/claims/evidence.go) — so this reads wider than the workable
// gather's open/in-progress filter deliberately, not by accident.
func claimStandings(ctx context.Context, stdout io.Writer, ap *app.App) (claims.Standings, model.Attribution, error) {
	cfg, err := config.Load(pathspec.New(ap.Workspace.RootDir))
	if err != nil {
		return nil, model.Attribution{}, err
	}
	allIssues, err := ap.Store.ListIssues(ctx, store.ListIssuesFilter{})
	if err != nil {
		return nil, model.Attribution{}, err
	}
	ids := make([]string, len(allIssues))
	for i, issue := range allIssues {
		ids[i] = issue.ID
	}
	relations, err := ap.Store.GetRelationsByIDs(ctx, ids)
	if err != nil {
		return nil, model.Attribution{}, err
	}
	parents := make(map[string]*model.Issue, len(allIssues))
	for _, issue := range allIssues {
		parents[issue.ID] = relations[issue.ID].Parent
	}
	events, err := ap.Store.ListAllEvents(ctx)
	if err != nil {
		return nil, model.Attribution{}, err
	}
	evidence, err := claims.NewEvidence(allIssues, parents, events)
	if err != nil {
		return nil, model.Attribution{}, err
	}
	local, err := ap.LocalCheckouts()
	if err != nil {
		// [LAW:no-silent-failure] This machine cannot prove which of its own
		// worktrees are still alive. The judgment call left open by
		// links-claims-1ihf.4's comment: fall back to the zero LocalCheckouts,
		// which voids nothing and lets freshness alone govern (leg 4's honest
		// degenerate case) — but say so, every time, because the fallback
		// silently changes which lanes route around this checkout otherwise.
		if _, printErr := fmt.Fprintf(stdout, "warning: could not enumerate local checkouts (%v) — claim liveness check skipped, freshness alone governs\n", err); printErr != nil {
			return nil, model.Attribution{}, printErr
		}
		local = claims.LocalCheckouts{}
	}
	fresh := claims.Freshness{Now: time.Now(), Window: cfg.Claims.FreshnessWindow}
	standings := claims.Derive(evidence, fresh, local)
	// NewAttribution collapses an absent stream (a checkout that has never
	// mutated) to the zero Attribution, which is exactly "no live claims" —
	// no branch needed here for the never-minted case.
	self := model.NewAttribution(ap.Stream.Value(), ap.Workspace.WorkspaceID)
	return standings, self, nil
}

// renderNextOutcome prints the row routeNext selected — or, for Exhausted
// and NoWork, returns the loud diagnostic instead of printing a ticket that
// was never picked. A fresh claim (EpicLane, Global) is announced before the
// row, visible at the moment the commitment happens
// (design-docs/work-claims.md, Routing step 3); an already-held claim
// (ServedFromClaim) prints exactly as `next` always has.
func renderNextOutcome(w io.Writer, outcome NextOutcome) (workflows.Occasion, error) {
	var row annotation.AnnotatedIssue
	var announce string
	switch o := outcome.(type) {
	case ServedFromClaim:
		row = o.Row
	case ServedFromEpicLane:
		row = o.Row
		announce = fmt.Sprintf("continuing epic %s: starting %s claims %s\n", o.Epic, o.Row.ID, o.Lane)
	case ServedFromGlobal:
		row = o.Row
		announce = fmt.Sprintf("starting %s claims %s\n", o.Row.ID, o.Lane)
	case Exhausted:
		return workflows.Occasion{}, exhaustedError(o)
	case NoWork:
		return workflows.Occasion{}, errors.New("no ready work")
	default:
		panic(fmt.Sprintf("renderNextOutcome: unhandled NextOutcome %T", outcome))
	}
	if announce != "" {
		if _, err := io.WriteString(w, announce); err != nil {
			return workflows.Occasion{}, err
		}
	}
	if err := printNextSummary(w, row); err != nil {
		return workflows.Occasion{}, err
	}
	return nextPulledOccasion(row.Issue), nil
}
