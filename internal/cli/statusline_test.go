package cli

import (
	"reflect"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/statusline"
)

// Each `lit next` outcome tells the file what it holds: the pick when the
// question was the plain one, and the checkout's own work in flight whatever
// narrowed it.
func TestNextOutcomesBecomeStatuslineFacts(t *testing.T) {
	row := annotation.AnnotatedIssue{Issue: model.Issue{ID: "t-1", Title: "One"}}
	ticket := statusline.Ticket{ID: "t-1", Title: "One"}
	cases := []struct {
		name    string
		outcome NextOutcome
		plain   bool
		want    statusline.Facts
	}{
		{"a fresh pick", ServedFromNewLane{Row: row}, true, statusline.Facts{statusline.Picked{Ticket: &ticket}}},
		{"a narrowed fresh pick", ServedFromNewLane{Row: row}, false, nil},
		{"own work handed back", ResumedOwnWork{Row: row}, true, statusline.Facts{statusline.Started{Ticket: ticket}, statusline.Picked{Ticket: &ticket}}},
		{"own work handed back to a narrowed question", ResumedOwnWork{Row: row}, false, statusline.Facts{statusline.Started{Ticket: ticket}}},
		{"nothing to pick", NoWork{}, true, statusline.Facts{statusline.Picked{}}},
		{"a scope with nothing left", Exhausted{}, true, statusline.Facts{statusline.Picked{}}},
		{"nothing to pick for a narrowed question", NoWork{}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextStatuslineFacts(tc.outcome, tc.plain); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("nextStatuslineFacts(%T, plain=%v) = %#v, want %#v", tc.outcome, tc.plain, got, tc.want)
			}
		})
	}
}

// Every change leaves the file showing the ticket's current title. A start
// takes the ticket on, every other transition invalidates what the file says
// about it, and a change of fields alone does neither.
func TestChangesBecomeStatuslineFacts(t *testing.T) {
	issue := model.Issue{ID: "t-1", Title: "One"}
	ticket := statusline.Ticket{ID: "t-1", Title: "One"}
	retitled := statusline.Retitled{Ticket: ticket}
	if got, want := changeStatuslineFacts(nil, issue), (statusline.Facts{retitled}); !reflect.DeepEqual(got, want) {
		t.Fatalf("fields alone = %#v, want %#v", got, want)
	}
	if got, want := changeStatuslineFacts(model.Start{Assignee: "me"}, issue), (statusline.Facts{retitled, statusline.Started{Ticket: ticket}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("start = %#v, want %#v", got, want)
	}
	for _, action := range []model.Action{model.Done{}, model.Close{Outcome: model.Wontfix{}}, model.Reopen{}, model.Archive{}, model.Unarchive{}, model.Delete{}, model.Restore{}} {
		if got, want := changeStatuslineFacts(action, issue), (statusline.Facts{retitled, statusline.Invalidated{ID: "t-1"}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("%T = %#v, want %#v", action, got, want)
		}
	}
}
