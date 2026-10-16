package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/statusline"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// recordStatusline writes the facts a command recorded to its checkout's
// statusline file, and puts what the write did on the command's event. A
// failed write is a warning and never the command's failure: the command's own
// work is already done, and the file is a record of it, not part of it.
func recordStatusline(ctx context.Context, ws workspace.Info, facts statusline.Facts) {
	out, err := statusline.Write(statusline.LocationOf(ws.RootDir, ws.GitCommonDir), facts, time.Now())
	recordStatuslineEvent(ctx, out, err)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: statusline file not written: %v\n", err)
		return
	}
	if out.Replaced != nil {
		fmt.Fprintf(os.Stderr, "warning: statusline file was unreadable and has been rewritten: %v\n", out.Replaced)
	}
}

// recordStatuslineEvent puts one statusline write on the command's event.
// Every key is set on every command that passes through the write, so a
// command that wrote nothing reads as zero bytes and no slots changed, not as
// a command the file never heard from. The record is in memory: it adds no
// store access and no lock to the write. [LAW:nothing-unseen]
func recordStatuslineEvent(ctx context.Context, out statusline.Outcome, err error) {
	telemetry.Count(ctx, "statusline.bytes", out.Bytes)
	telemetry.Set(ctx, "statusline.changed", out.Changed)
	telemetry.Set(ctx, "statusline.created", out.Created)
	telemetry.Set(ctx, "statusline.exclude_added", out.ExcludeAdded)
	telemetry.Set(ctx, "statusline.replaced", errorText(out.Replaced))
	telemetry.Set(ctx, "statusline.error", errorText(err))
}

// errorText is err's message, or nil for no error, as the event records it.
func errorText(err error) *string {
	if err == nil {
		return nil
	}
	text := err.Error()
	return &text
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
	case ServedPastNarrowing:
		picked = statuslineTicketRef(o.Row.Issue)
	case Exhausted, Narrowed, NoWork:
	default:
		panic(fmt.Sprintf("nextStatuslineFacts: unhandled NextOutcome %T", outcome))
	}
	if plain {
		facts.Record(statusline.Picked{Ticket: picked})
	}
	return facts
}

func statuslineTicketRef(issue model.Issue) *statusline.Ticket {
	ticket := app.StatuslineTicket(issue)
	return &ticket
}
