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
// printed one raw would start a line with that text, or hand the terminal its
// ESC; held to its row, a break prints as model.QuotedBreak in front of it and the
// ESC as U+FFFD. [LAW:behavior-not-structure]
func TestEveryListingHoldsAStoredBreakToItsRow(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("epic"), Lane: storage.SharedLane, Topic: "demo", IssueType: model.TypeEpic})
	open := h.createIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("open"), Topic: "demo", ParentID: epic.ID, Lane: storage.MustParseLane("left")})
	stale := h.createIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("stale"), Topic: "demo", ParentID: epic.ID, Lane: storage.MustParseLane("right")})
	upstreamEpic := h.createIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("upstream epic"), Lane: storage.SharedLane, Topic: "demo", IssueType: model.TypeEpic})
	upstream := h.createIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("upstream"), Lane: storage.SharedLane, Topic: "demo", ParentID: upstreamEpic.ID})
	h.addDependency(stale.ID, upstream.ID)

	raw, err := storage.TestSupport.Of(h.ap.Store)
	if err != nil {
		t.Fatalf("test support: %v", err)
	}
	plant := func(id model.ID, column, value string) {
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

	policy := workspaceReadyPolicy(runEnv(t), h.ap)
	command := func(run func(testing.TB, context.Context, io.Writer, *app.App, []string) error, args ...string) func(io.Writer) error {
		return func(w io.Writer) error { return run(t, h.ctx, w, h.ap, args) }
	}
	// check runs one listing. held names the planted values it must print held
	// to their row; with none named, any one planted value will do.
	check := func(name string, print func(io.Writer) error, held ...string) {
		t.Helper()
		if len(held) == 0 {
			held = []string{model.QuotedBreak + forged}
		}
		var out bytes.Buffer
		if err := print(&out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		printed := out.String()
		for _, value := range held {
			if !strings.Contains(printed, value) {
				t.Errorf("%s printed no planted value held to its row (want %q in it):\n%s", name, value, printed)
			}
		}
		// No break form splits at an ESC, so a raw one is caught by its presence:
		// every ESC is a terminal control no listing means to print.
		if strings.ContainsRune(printed, '\x1b') {
			t.Errorf("%s printed a planted ESC raw:\n%q", name, printed)
		}
		// A planted id's tab, printed raw, would add a cell to its row. A quoted
		// block of remote text ("  | " rows) is DATA, not cells, and keeps it.
		for _, line := range strings.Split(printed, "\n") {
			if !strings.HasPrefix(line, "  | ") && strings.Contains(line, forged+"\t") {
				t.Errorf("%s printed a planted tab raw: %q\n%q", name, line, printed)
			}
		}
		// Split at every break form a reader may take for one, not only \n.
		for _, line := range strings.Split(model.LineBreakNormalizer.Replace(printed), "\n") {
			if strings.HasPrefix(strings.TrimLeft(line, " |"), forged) {
				t.Errorf("%s printed a planted continuation as a row of its own: %q\n%s", name, line, printed)
			}
		}
	}

	// Asked before anything is in progress, next serves the laned ticket, so
	// its advice names the planted lane.
	check("next", command(runNext), "left"+model.QuotedBreak+forged+" lane")
	h.applyAction(stale.ID, model.Start{Assignee: "tester"}, "")
	h.backdateUpdatedAt(stale.ID, 7*time.Hour)
	// Free text beyond title and lane: an assignee and a topic, planted the
	// same way, so show's header and assignee line are held as the listings are.
	plant(stale.ID, "assignee", "tester\u2029"+forged+" assignee")
	plant(open.ID, "topic", "demo\r"+forged+" topic")
	// An id is minted by lit but carried by sync and import, so a stored one
	// can hold a break and a tab too. The epic and a child under it are renamed
	// in every row that names them, so the parent edge, the history and the
	// epic context all reach the planted ids.
	rename := func(issue *model.Issue, to model.ID) {
		t.Helper()
		for _, stmt := range []string{
			"UPDATE issues SET id = ? WHERE id = ?",
			"UPDATE relations SET src_id = ? WHERE src_id = ?",
			"UPDATE relations SET dst_id = ? WHERE dst_id = ?",
			"UPDATE issue_events SET issue_id = ? WHERE issue_id = ?",
			"UPDATE comments SET issue_id = ? WHERE issue_id = ?",
			"UPDATE labels SET issue_id = ? WHERE issue_id = ?",
			"UPDATE plan_reviews SET issue_id = ? WHERE issue_id = ?",
		} {
			if err := raw.ExecRawForTest(h.ctx, stmt, to, issue.ID); err != nil {
				t.Fatalf("rename %s to %q: %s: %v", issue.ID, to, stmt, err)
			}
		}
		issue.ID = to
	}
	if err := raw.ExecRawForTest(h.ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatalf("foreign key checks off: %v", err)
	}
	rename(&epic, model.ID(epic.ID.Raw()+"\n"+forged+"\tepic-id"))
	rename(&open, model.ID(open.ID.Raw()+" "+forged+"\topen-id"))
	if err := raw.ExecRawForTest(h.ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		t.Fatalf("foreign key checks on: %v", err)
	}
	// held, a planted id printed through its type: the break a token and the
	// tab a space, so it neither starts a row nor adds a cell to one.
	epicID := model.QuotedBreak + forged + " epic-id"
	openID := model.QuotedBreak + forged + " open-id"
	for _, listing := range []struct {
		name  string
		print func(io.Writer) error
	}{
		{"ls", func(w io.Writer) error { return runListWithStore(t, h.ctx, w, h.ap.Store, policy, nil) }},
		{"ls --format table", func(w io.Writer) error {
			return runListWithStore(t, h.ctx, w, h.ap.Store, policy, []string{"--format", "table"})
		}},
		{"backlog", func(w io.Writer) error { return runWorkable(t, h.ctx, w, h.ap, nil, backlogView) }},
		{"children", command(runChildren, epic.ID.Raw())},
		{"show (leaf)", command(runShow, stale.ID.Raw())},
		{"show (lane)", command(runShow, open.ID.Raw())},
		{"show (epic)", command(runShow, epic.ID.Raw())},
		{"show --field title,lane", command(runShow, open.ID.Raw(), "--field", "title,lane")},
		{"orphaned", command(runOrphaned)},
		{"restore's removed list", func(w io.Writer) error {
			return writeRemoved(w, []storage.Removed{{IssueID: open.ID, Title: held.Title}, {IssueID: epic.ID, Title: parent.Title}}, "/backups/x.json")
		}},
		{"contested lanes", func(w io.Writer) error { return printContestedLanes(w, contested, time.Now()) }},
		// Last: the write's summary row reprints the stored title it left alone.
		{"update's summary", command(runUpdate, open.ID.Raw(), "--priority", "urgent")},
	} {
		check(listing.name, listing.print)
	}
	check("show's assignee line", command(runShow, stale.ID.Raw()), "tester"+model.QuotedBreak+forged+" assignee")
	check("show's topic line", command(runShow, open.ID.Raw()), "demo"+model.QuotedBreak+forged+" topic")

	// Each listing that names a planted id prints it held.
	check("ls's ids", func(w io.Writer) error { return runListWithStore(t, h.ctx, w, h.ap.Store, policy, nil) }, epicID, openID)
	check("ls --format table's ids", func(w io.Writer) error {
		return runListWithStore(t, h.ctx, w, h.ap.Store, policy, []string{"--format", "table"})
	}, epicID, openID)
	check("ls --columns id,parent's ids", func(w io.Writer) error {
		return runListWithStore(t, h.ctx, w, h.ap.Store, policy, []string{"--columns", "id,parent"})
	}, epicID, openID)
	check("backlog's ids", func(w io.Writer) error { return runWorkable(t, h.ctx, w, h.ap, nil, backlogView) }, openID)
	check("children's ids", command(runChildren, epic.ID.Raw()), openID)
	check("show (leaf)'s epic context", command(runShow, stale.ID.Raw()), epicID, openID)
	check("show (lane)'s ids", command(runShow, open.ID.Raw()), openID, epicID)
	check("show (epic)'s ids", command(runShow, epic.ID.Raw()), epicID, openID)
	check("show --field id's id", command(runShow, open.ID.Raw(), "--field", "id"), openID)
	check("history's id", command(runHistory, open.ID.Raw()), openID)
	check("update's summary id", command(runUpdate, open.ID.Raw(), "--priority", "normal"), openID)
	h.applyAction(open.ID, model.Start{Assignee: "tester"}, "")
	check("next's in-progress advice", command(runNext), openID)
}
