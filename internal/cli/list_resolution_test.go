package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestListStateCellNamesTheCloseReason pins the close reason into `lit ls`'s
// default projection. A column of identical "closed" cells reads a wontfix
// declination as finished work, and the loss is directional: it can only make
// a body of work look more finished than it is, so nothing prompts a reader to
// check. Every closed shape is driven through the real listing, plus one frozen
// row, because the reason (":") and the retention axis ("+") share one cell and
// must stay distinguishable in it.
// [LAW:behavior-not-structure] the contract is the rendered row a reader acts on.
func TestListStateCellNamesTheCloseReason(t *testing.T) {
	h := newReadyTestHarness(t)
	canonical := h.createIssue(storage.CreateIssueInput{Title: "the canonical ticket", IssueType: model.TypeTask, Topic: "listing"})

	cases := []struct {
		title   string
		actions []model.Action
		want    string
	}{
		{title: "finished", actions: []model.Action{model.Done{}}, want: "closed"},
		{title: "duplicated", actions: []model.Action{model.Close{Outcome: model.Duplicate{Of: canonical.ID}}}, want: "closed:duplicate"},
		{title: "replaced", actions: []model.Action{model.Close{Outcome: model.Superseded{By: canonical.ID}}}, want: "closed:superseded"},
		{title: "overtaken", actions: []model.Action{model.Close{Outcome: model.Obsolete{}}}, want: "closed:obsolete"},
		{title: "declined", actions: []model.Action{model.Close{Outcome: model.Wontfix{}}}, want: "closed:wontfix"},
		{title: "declined and shelved", actions: []model.Action{model.Close{Outcome: model.Wontfix{}}, model.Archive{}}, want: "closed:wontfix+archived"},
	}
	rows := make([]string, len(cases))
	for i, tc := range cases {
		issue := h.createIssue(storage.CreateIssueInput{Title: tc.title, IssueType: model.TypeTask, Topic: "listing"})
		for _, action := range tc.actions {
			if _, err := h.ap.Store.Apply(h.ctx, issue.ID, storage.Change{Action: action, Actor: "tester"}); err != nil {
				t.Fatalf("Apply(%s, %T) error = %v", tc.title, action, err)
			}
		}
		rows[i] = issue.ID + " | " + tc.want + " | listing | " + tc.title
	}

	// Exact lines, so a "closed" expectation cannot be met by a
	// "closed:wontfix" row's prefix.
	got := strings.Split(runLs(t, h.ap, "--status", "closed", "--include-archived"), "\n")
	for i, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			if !slices.Contains(got, rows[i]) {
				t.Fatalf("ls --status closed missing row %q; output:\n%s", rows[i], strings.Join(got, "\n"))
			}
		})
	}
}

// TestCloseSummarySharesTheStateCell pins the one-line summary `lit close`
// prints to the same cell the listing shows, so the reply to the close and
// the later listing name the reason identically.
func TestCloseSummarySharesTheStateCell(t *testing.T) {
	h := newReadyTestHarness(t)
	issue := h.createIssue(storage.CreateIssueInput{Title: "declined", IssueType: model.TypeTask, Topic: "listing"})

	var out bytes.Buffer
	if err := runTransition(h.ctx, &out, h.ap, []string{issue.ID, "--resolution", "wontfix"}, closeSpec); err != nil {
		t.Fatalf("runTransition(close wontfix) error = %v", err)
	}
	want := issue.ID + " [closed:wontfix/task/listing/normal] declined"
	if !slices.Contains(strings.Split(out.String(), "\n"), want) {
		t.Fatalf("lit close output missing summary %q; output:\n%s", want, out.String())
	}
}
