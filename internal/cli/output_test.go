package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// TestShowOmitsHistoryTrailWhileHistoryViewRendersIt pins the split the
// show-history epic delivers: fed one identical multi-edit IssueDetail,
// `lit show` (printIssueDetail) renders the ticket's CURRENT fields and NOT the
// field-level `from → to` change-log, while `lit history` (printIssueHistory)
// renders that trail in full. Asserting both formatters against the same input
// makes "same data, two views" the enforced contract [LAW:behavior-not-structure]:
// a reader trusts show as current state, and the trail still has a home. Local
// tz is pinned so the trail's timestamp format stays covered where it now lives.
func TestShowOmitsHistoryTrailWhileHistoryViewRendersIt(t *testing.T) {
	// serial: no t.Parallel — rewrites the process-global time.Local;
	// parallel readers of it would race.
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	previousLocal := time.Local
	time.Local = denver
	t.Cleanup(func() {
		time.Local = previousLocal
	})

	issue, err := model.HydrateStatus(model.Issue{
		ID:        "links-test.1",
		Title:     "Current title",
		IssueType: "task",
		Topic:     "history",
		CreatedAt: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC),
	}, model.StatusView{Value: model.StateOpen})
	if err != nil {
		t.Fatalf("HydrateStatus() error = %v", err)
	}

	// One edit that renamed the title: its trail carries a `from → to` change line
	// and the current fields already hold the latest value.
	detail := model.IssueDetail{
		Issue: issue,
		Events: []model.IssueEvent{{
			Action:    "update",
			Reason:    "renamed",
			Actor:     "alice",
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			Changes: []model.FieldChange{{
				Field: "title", From: "Old title", To: "Current title",
			}},
		}},
	}
	changeLine := "title: Old title → Current title"

	var show bytes.Buffer
	if err := printIssueDetail(&show, detail); err != nil {
		t.Fatalf("printIssueDetail() error = %v", err)
	}
	if !strings.Contains(show.String(), "Current title") {
		t.Fatalf("lit show dropped the current title in:\n%s", show.String())
	}
	if strings.Contains(show.String(), "history:") {
		t.Fatalf("lit show still prints a history block in:\n%s", show.String())
	}
	if strings.Contains(show.String(), changeLine) {
		t.Fatalf("lit show still prints the field change-log in:\n%s", show.String())
	}
	if strings.Contains(show.String(), "→") {
		t.Fatalf("lit show still prints a field-change arrow in:\n%s", show.String())
	}

	var history bytes.Buffer
	if err := printIssueHistory(&history, detail); err != nil {
		t.Fatalf("printIssueHistory() error = %v", err)
	}
	if !strings.Contains(history.String(), changeLine) {
		t.Fatalf("lit history dropped the transition trail in:\n%s", history.String())
	}
	if want := "- [alice @ Jan 1, 2026 8:04 PM MST] update renamed"; !strings.Contains(history.String(), want) {
		t.Fatalf("lit history missing timestamped event line %q in:\n%s", want, history.String())
	}
}

// TestPrintIssueGroupNamesRetentionRatherThanStatus pins the relationship
// groups to issueStanding: the two lifecycle axes are orthogonal, and a
// soft-deleted ticket's status is still "open", so printing State() alone
// rendered a dead blocker as "[open]" and sent the reader hunting for an id that
// appears in no listing (links-readiness-9no1). Retention dominates because a
// frozen issue's status describes work nobody may do.
// [LAW:behavior-not-structure] The contract asserted is the rendered line a
// reader acts on, not which accessor produced the word.
func TestPrintIssueGroupNamesRetentionRatherThanStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    model.State
		retention model.Retention
		want      string
	}{
		{name: "open", status: model.StateOpen, retention: model.Live{}, want: "[open]"},
		{name: "closed", status: model.StateClosed, retention: model.Live{}, want: "[closed]"},
		{name: "archived", status: model.StateOpen, retention: model.Archived{At: time.Now()}, want: "[archived]"},
		// The status axis reads open on both frozen arms: that disagreement is
		// the whole bug, so the arms differ only in retention.
		{name: "deleted", status: model.StateOpen, retention: model.Deleted{At: time.Now()}, want: "[deleted]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dep, err := model.HydrateStatus(model.Issue{
				ID: "links-test.1", Title: "The blocker", IssueType: "task", Topic: "readiness",
			}, model.StatusView{Value: tc.status})
			if err != nil {
				t.Fatalf("HydrateStatus() error = %v", err)
			}
			dep.SetRetention(tc.retention)

			var buf bytes.Buffer
			if err := printIssueGroup(&buf, "depends_on", []model.Issue{dep}); err != nil {
				t.Fatalf("printIssueGroup() error = %v", err)
			}

			want := "- links-test.1 " + tc.want + " The blocker\n"
			if !strings.Contains(buf.String(), want) {
				t.Fatalf("printIssueGroup() = %q, want a line %q", buf.String(), want)
			}
		})
	}
}
