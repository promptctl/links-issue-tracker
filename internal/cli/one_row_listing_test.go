package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// forged opens every continuation planted below: text that, printed at
// column 0, would read as a row lit never wrote.
const forged = "FORGED"

// TestEveryListingHoldsAStoredBreakToItsRow pins that a title or lane already
// stored holding a line break prints on the row it belongs to, in every
// listing. The values are planted under the write path, as data older than
// any write gate is: each title and the lane hold a different break form, or
// an ESC a terminal can turn into one, followed by forged text. A listing that
// printed one raw would start a line with that text; held to its row, the
// break prints as quotedBreak in front of it. [LAW:behavior-not-structure]
func TestEveryListingHoldsAStoredBreakToItsRow(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Title: "epic", Topic: "demo", IssueType: model.TypeEpic})
	open := h.createIssue(storage.CreateIssueInput{Title: "open", Topic: "demo", ParentID: epic.ID, Lane: "left"})
	stale := h.createIssue(storage.CreateIssueInput{Title: "stale", Topic: "demo", ParentID: epic.ID, Lane: "right"})
	upstreamEpic := h.createIssue(storage.CreateIssueInput{Title: "upstream epic", Topic: "demo", IssueType: model.TypeEpic})
	upstream := h.createIssue(storage.CreateIssueInput{Title: "upstream", Topic: "demo", ParentID: upstreamEpic.ID})
	h.addDependency(stale.ID, upstream.ID)

	raw, err := storage.TestSupport.Of(h.ap.Store)
	if err != nil {
		t.Fatalf("test support: %v", err)
	}
	plant := func(id, column, value string) {
		t.Helper()
		if err := raw.ExecRawForTest(h.ctx, "UPDATE issues SET "+column+" = ? WHERE id = ?", value, id); err != nil {
			t.Fatalf("plant %s of %s: %v", column, id, err)
		}
	}
	plant(epic.ID, "title", "epic\n"+forged+" epic")
	plant(open.ID, "title", "open\r\n"+forged+" open")
	plant(open.ID, "lane", "left\u2028"+forged+" lane")
	plant(stale.ID, "title", "stale\u0085"+forged+" stale")
	plant(upstream.ID, "title", "upstream\v"+forged+" upstream")
	plant(upstreamEpic.ID, "title", "upstream epic\x1bE"+forged+" upstream epic")

	held := h.issueDetail(open.ID)
	parent := h.issueDetail(epic.ID)
	lane := model.LaneOf(held, &parent)
	contested := claimContext{standings: claims.Standings{lane: claims.Held{
		Tenure:    claims.Tenure{By: model.NewAttribution("aaaaaaaabbbb", "ws-local")},
		Contested: []model.Attribution{model.NewAttribution("eeeeeeeeffff", "ws-local")},
	}}}

	policy := workspaceReadyPolicy(testEnv(), h.ap)
	command := func(run func(context.Context, io.Writer, *app.App, []string) error, args ...string) func(io.Writer) error {
		return func(w io.Writer) error { return run(h.ctx, w, h.ap, args) }
	}
	check := func(name string, print func(io.Writer) error) {
		t.Helper()
		var out bytes.Buffer
		if err := print(&out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		printed := out.String()
		if !strings.Contains(printed, quotedBreak+forged) {
			t.Errorf("%s printed no planted value held to its row (want %q in it):\n%s", name, quotedBreak+forged, printed)
		}
		// Split at every break form a reader may take for one, not only \n.
		for _, line := range strings.Split(lineBreakNormalizer.Replace(printed), "\n") {
			if strings.HasPrefix(strings.TrimLeft(line, " |"), forged) {
				t.Errorf("%s printed a planted continuation as a row of its own: %q\n%s", name, line, printed)
			}
		}
	}

	// Asked before anything is in progress, next serves the laned ticket, so
	// its advice names the planted lane.
	check("next", command(runNext))
	h.applyAction(stale.ID, model.Start{Assignee: "tester"}, "")
	h.backdateUpdatedAt(stale.ID, 7*time.Hour)
	for _, listing := range []struct {
		name  string
		print func(io.Writer) error
	}{
		{"ls", func(w io.Writer) error { return runListWithStore(h.ctx, w, h.ap.Store, policy, nil) }},
		{"ls --format table", func(w io.Writer) error {
			return runListWithStore(h.ctx, w, h.ap.Store, policy, []string{"--format", "table"})
		}},
		{"backlog", func(w io.Writer) error { return runWorkable(h.ctx, w, h.ap, nil, backlogView) }},
		{"children", command(runChildren, epic.ID)},
		{"show (leaf)", command(runShow, stale.ID)},
		{"show (lane)", command(runShow, open.ID)},
		{"show (epic)", command(runShow, epic.ID)},
		{"show --field title,lane", command(runShow, open.ID, "--field", "title,lane")},
		{"orphaned", command(runOrphaned)},
		{"restore's removed list", func(w io.Writer) error {
			return writeRemoved(w, []storage.Removed{{IssueID: open.ID, Title: held.Title}, {IssueID: epic.ID, Title: parent.Title}}, "/backups/x.json")
		}},
		{"contested lanes", func(w io.Writer) error { return printContestedLanes(w, contested, time.Now()) }},
		// Last: the write's summary row reprints the stored title it left alone.
		{"update's summary", command(runUpdate, open.ID, "--priority", "urgent")},
	} {
		check(listing.name, listing.print)
	}
}
