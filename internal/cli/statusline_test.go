package cli

import (
	"reflect"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/statusline"
)

// Each `lit next` outcome tells the file what it holds: the pick when the
// question was the plain one, and the checkout's own work in flight, with its
// rollup progress and its phase, whatever narrowed it.
func TestNextOutcomesBecomeStatuslineFacts(t *testing.T) {
	epic := model.Issue{ID: "t-epic", IssueType: model.TypeEpic}
	inFlight := hydratedIssue(t, "t-1", nil, model.StateInProgress)
	inFlight.Title = "One"
	lanemates := []model.Issue{inFlight, hydratedIssue(t, "t-2", nil, model.StateClosed), hydratedIssue(t, "t-3", nil, model.StateOpen)}
	parents := map[string]*model.Issue{}
	for _, issue := range lanemates {
		parents[issue.ID] = &epic
	}
	// The in-flight ticket's history moves it to review; a lanemate's newer
	// event names another phase, which is not the in-flight ticket's.
	events := []model.IssueEvent{
		{ID: "e-1", IssueID: "t-1", Action: string(model.ActionPhase), Changes: []model.FieldChange{{Field: model.PhaseField, From: "plan", To: "review"}}, CreatedAt: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)},
		{ID: "e-2", IssueID: "t-3", Action: string(model.ActionPhase), Changes: []model.FieldChange{{Field: model.PhaseField, From: "plan", To: "merge"}}, CreatedAt: time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)},
	}
	evidence, err := claims.NewEvidence(lanemates, parents, events)
	if err != nil {
		t.Fatal(err)
	}
	row := annotation.AnnotatedIssue{Issue: inFlight}
	resumed := ResumedOwnWork{Row: row}
	ticket := statusline.Ticket{ID: "t-1", Title: "One"}
	started := statusline.Started{Ticket: ticket, Progress: statusline.RollupProgress{CompletedCount: 1, TotalCount: 3}}
	phased := statusline.PhaseRead{ID: "t-1", Phase: "review"}
	cases := []struct {
		name    string
		outcome NextOutcome
		plain   bool
		want    statusline.Facts
	}{
		{"a fresh pick", ServedFromNewLane{Row: row}, true, statusline.Facts{statusline.Picked{Ticket: &ticket}}},
		{"a narrowed fresh pick", ServedFromNewLane{Row: row}, false, nil},
		{"own work handed back", resumed, true, statusline.Facts{started, phased, statusline.Picked{Ticket: &ticket}}},
		{"own work handed back to a narrowed question", resumed, false, statusline.Facts{started, phased}},
		{"nothing to pick", NoWork{}, true, statusline.Facts{statusline.Picked{}}},
		{"a scope with nothing left", Exhausted{}, true, statusline.Facts{statusline.Picked{}}},
		{"nothing to pick for a narrowed question", NoWork{}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextStatuslineFacts(tc.outcome, tc.plain, evidence); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("nextStatuslineFacts(%T, plain=%v) = %#v, want %#v", tc.outcome, tc.plain, got, tc.want)
			}
		})
	}
}
