package cli

import (
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

	for _, tc := range []struct {
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
	} {
		issue := h.createIssue(storage.CreateIssueInput{Title: tc.title, IssueType: model.TypeTask, Topic: "listing"})
		for _, action := range tc.actions {
			if _, err := h.ap.Store.Apply(h.ctx, issue.ID, storage.Change{Action: action, Actor: "tester"}); err != nil {
				t.Fatalf("Apply(%s, %T) error = %v", tc.title, action, err)
			}
		}
		t.Run(tc.title, func(t *testing.T) {
			got := runLs(t, h.ap, "--status", "closed", "--include-archived")
			want := issue.ID + " | " + tc.want + " | listing | " + tc.title
			if !hasLine(got, want) {
				t.Fatalf("ls --status closed missing row %q; output:\n%s", want, got)
			}
		})
	}
}

// hasLine reports whether out has a line exactly equal to want, so a
// "closed" expectation cannot be satisfied by a "closed:wontfix" row's prefix.
func hasLine(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			return true
		}
	}
	return false
}
