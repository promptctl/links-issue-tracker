package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/statusline"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// recordStatusline writes the facts a command recorded to its checkout's
// statusline file. A failed write is a warning and never the command's
// failure: the command's own work is already done, and the file is a record of
// it, not part of it.
func recordStatusline(ws workspace.Info, facts statusline.Facts) {
	out, err := statusline.Write(statusline.LocationOf(ws.RootDir, ws.GitCommonDir), facts, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: statusline file not written: %v\n", err)
		return
	}
	if out.Replaced != nil {
		fmt.Fprintf(os.Stderr, "warning: statusline file was unreadable and has been rewritten: %v\n", out.Replaced)
	}
}

func statuslineTicket(issue model.Issue) statusline.Ticket {
	return statusline.Ticket{ID: issue.ID, Title: issue.Title}
}

// applyChange applies one change to a ticket and records what it tells the
// statusline file. Every command that changes a ticket does so through here,
// so the file hears of each change without any command remembering to tell it.
// [LAW:single-enforcer]
func applyChange(ctx context.Context, ap *app.App, issueID string, change storage.Change) (model.Issue, error) {
	issue, err := ap.Store.Apply(ctx, issueID, change)
	if err != nil {
		return model.Issue{}, err
	}
	ap.Statusline.Record(changeStatuslineFacts(change.Action, issue)...)
	return issue, nil
}

// changeStatuslineFacts is what a change tells the statusline file. Every
// change leaves the ticket with the title it now has. A start is this checkout
// taking the ticket on; every other transition moves the ticket out of what
// the file can say about it; a change of fields alone moves it nowhere.
func changeStatuslineFacts(action model.Action, issue model.Issue) statusline.Facts {
	ticket := statuslineTicket(issue)
	facts := statusline.Facts{statusline.Retitled{Ticket: ticket}}
	switch action.(type) {
	case nil:
	case model.Start:
		facts.Record(statusline.Started{Ticket: ticket})
	default:
		facts.Record(statusline.Invalidated{ID: issue.ID})
	}
	return facts
}

// nextStatuslineFacts is what a `lit next` outcome tells the statusline file.
// Work handed back to this checkout is what it is working on, whatever narrowed
// the question. The pick is "what's next" only when the question was the plain
// one: a filtered or --all `lit next` answers a different question.
func nextStatuslineFacts(outcome NextOutcome, plain bool) statusline.Facts {
	var facts statusline.Facts
	var picked *statusline.Ticket
	switch o := outcome.(type) {
	case ResumedOwnWork:
		picked = statuslineTicketRef(o.Row.Issue)
		facts.Record(statusline.Started{Ticket: *picked})
	case ServedFromClaim:
		picked = statuslineTicketRef(o.Row.Issue)
	case ServedFromEpicLane:
		picked = statuslineTicketRef(o.Row.Issue)
	case ServedFromNewLane:
		picked = statuslineTicketRef(o.Row.Issue)
	case ServedFromDependency:
		picked = statuslineTicketRef(o.Row.Issue)
	case ServedPastExhaustion:
		picked = statuslineTicketRef(o.Row.Issue)
	case Exhausted, NoWork:
	default:
		panic(fmt.Sprintf("nextStatuslineFacts: unhandled NextOutcome %T", outcome))
	}
	if plain {
		facts.Record(statusline.Picked{Ticket: picked})
	}
	return facts
}

func statuslineTicketRef(issue model.Issue) *statusline.Ticket {
	ticket := statuslineTicket(issue)
	return &ticket
}
