package cli

import (
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
)

// noTicket is what a body's <ticket> reads on a run that handed no ticket back.
const noTicket = "No ticket: nothing this run asked about is ready to start from this checkout."

// nextOccasion is the occasion `lit next` fires, on every run. The text bound
// to `next` is everything the command says, so this supplies only what that
// text cannot know: the ticket routing handed back, as `lit backlog` shows it,
// or the statement that it handed none back. What to do in either case is the
// text's to say. [LAW:one-source-of-truth]
func nextOccasion(outcome NextOutcome, details map[model.ID]storage.IssueRelations, cc claimContext) workflows.Occasion {
	row, served := routedRow(outcome)
	if !served {
		return workflows.Occasion{Event: workflows.EventNext, Data: map[string]string{"ticket": noTicket}}
	}
	var ticket strings.Builder
	// printNextSummary writes into a strings.Builder, which cannot fail.
	_ = printNextSummary(&ticket, row, cc, model.LaneOf(row.Issue, details[row.ID].Parent))
	return workflows.Occasion{
		Event:   workflows.EventNext,
		IssueID: row.ID,
		Labels:  model.RawTexts(row.Issue.Labels),
		Data:    map[string]string{"ticket": strings.TrimRight(ticket.String(), "\n")},
	}
}

// routedRow is the row an outcome hands back, if it hands one back.
func routedRow(outcome NextOutcome) (annotation.AnnotatedIssue, bool) {
	switch o := outcome.(type) {
	case ServedFromClaim:
		return o.Row, true
	case ResumedOwnWork:
		return o.Row, true
	case ServedFromEpicLane:
		return o.Row, true
	case ServedFromNewLane:
		return o.Row, true
	case ServedFromDependency:
		return o.Row, true
	case ServedPastExhaustion:
		return o.Row, true
	case ServedPastNarrowing:
		return o.Row, true
	}
	return annotation.AnnotatedIssue{}, false
}
