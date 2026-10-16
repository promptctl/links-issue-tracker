package cli

import (
	"reflect"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/statusline"
)

// Each `lit next` outcome tells the file what it holds: the pick when the
// question was the plain one, and the checkout's own work in flight, with its
// lane's count, whatever narrowed it.
func TestNextOutcomesBecomeStatuslineFacts(t *testing.T) {
	epic := model.Issue{ID: "t-epic", IssueType: model.TypeEpic}
	inFlight := hydratedIssue(t, "t-1", nil, model.StateInProgress)
	inFlight.Title = "One"
	lanemates := []model.Issue{inFlight, hydratedIssue(t, "t-2", nil, model.StateClosed), hydratedIssue(t, "t-3", nil, model.StateOpen)}
	parents := map[string]*model.Issue{}
	for _, issue := range lanemates {
		parents[issue.ID] = &epic
	}
	evidence, err := claims.NewEvidence(lanemates, parents, nil)
	if err != nil {
		t.Fatal(err)
	}
	row := annotation.AnnotatedIssue{Issue: inFlight}
	resumed := ResumedOwnWork{Row: row, Lane: model.LaneOf(inFlight, &epic)}
	ticket := statusline.Ticket{ID: "t-1", Title: "One"}
	counted := statusline.LaneCounted{ID: "t-1", Lane: statusline.LaneProgress{Done: 1, Total: 3}}
	cases := []struct {
		name    string
		outcome NextOutcome
		plain   bool
		want    statusline.Facts
	}{
		{"a fresh pick", ServedFromNewLane{Row: row}, true, statusline.Facts{statusline.Picked{Ticket: &ticket}}},
		{"a narrowed fresh pick", ServedFromNewLane{Row: row}, false, nil},
		{"own work handed back", resumed, true, statusline.Facts{statusline.Started{Ticket: ticket}, counted, statusline.Picked{Ticket: &ticket}}},
		{"own work handed back to a narrowed question", resumed, false, statusline.Facts{statusline.Started{Ticket: ticket}, counted}},
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
