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
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
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
const nextUsage = "usage: lit next [--type ...] [--status ...] [--labels ...] [--assignee <user>]"

func runNext(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("next")
	assignee := fs.String("assignee", "", "Filter by assignee")
	issueType := fs.String("type", "", "Filter by issue type")
	status := fs.String("status", "", "Filter by status: open|in_progress")
	labels := fs.String("labels", "", "Comma-separated labels all of which must match")
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
	cc, err := gatherClaimContext(ctx, stdout, ap)
	if err != nil {
		return err
	}
	occasion, err := renderNextOutcome(stdout, routeNext(rows, details, cc.standings, cc.self), details, cc)
	if err != nil {
		return err
	}
	return workflows.Dispatch(stdout, os.Stderr, ap.Workspace, occasion)
}

// renderNextOutcome prints the row routeNext selected — or, for Exhausted
// and NoWork, returns the loud diagnostic instead of printing a ticket that
// was never picked. A fresh claim (EpicLane, Global) is announced before the
// row, visible at the moment the commitment happens
// (design-docs/work-claims.md, Routing step 3); an already-held claim
// (ServedFromClaim) prints exactly as `next` always has.
func renderNextOutcome(w io.Writer, outcome NextOutcome, details map[string]storage.IssueRelations, cc claimContext) (workflows.Occasion, error) {
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
	lane := model.LaneOf(row.Issue, details[row.ID].Parent)
	if err := printNextSummary(w, row, cc, lane); err != nil {
		return workflows.Occasion{}, err
	}
	return nextPulledOccasion(row.Issue), nil
}
