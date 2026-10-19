package app

import (
	"reflect"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/statusline"
)

// Every change leaves the file showing the ticket's current title. A start
// says no more than that, since `lit start` records it with its lane's count;
// a phase move puts it in the phase the move names, every other transition
// invalidates what the file says about it, and a change of fields alone does
// none of these.
func TestChangesBecomeStatuslineFacts(t *testing.T) {
	issue := model.Issue{ID: "t-1", Title: "One"}
	ticket := statusline.Ticket{ID: "t-1", Title: "One"}
	retitled := statusline.Retitled{Ticket: ticket}
	if got, want := changeStatuslineFacts(nil, issue), (statusline.Facts{retitled}); !reflect.DeepEqual(got, want) {
		t.Fatalf("fields alone = %#v, want %#v", got, want)
	}
	if got, want := changeStatuslineFacts(model.Start{Assignee: "me"}, issue), (statusline.Facts{retitled}); !reflect.DeepEqual(got, want) {
		t.Fatalf("start = %#v, want %#v", got, want)
	}
	if got, want := changeStatuslineFacts(model.SetPhase{Phase: model.PhaseReview}, issue), (statusline.Facts{retitled, statusline.PhaseRead{ID: "t-1", Phase: "review"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("phase move = %#v, want %#v", got, want)
	}
	for _, action := range []model.Action{model.Done{}, model.Close{Outcome: model.Wontfix{}}, model.Reopen{}, model.Archive{}, model.Unarchive{}, model.Delete{}, model.Restore{}} {
		if got, want := changeStatuslineFacts(action, issue), (statusline.Facts{retitled, statusline.Invalidated{ID: "t-1"}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("%T = %#v, want %#v", action, got, want)
		}
	}
}
