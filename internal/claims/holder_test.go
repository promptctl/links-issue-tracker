package claims_test

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// actorEvent is event with the actor the store stamps on it.
func actorEvent(id, issueID model.ID, action model.ActionName, at time.Time, by model.Attribution, actor model.Text) model.IssueEvent {
	e := event(id, issueID, action, at, by)
	e.Actor = actor
	return e
}

// TestTheHolderIsTheCheckoutWhoeverActsInIt: a lane's holder is the checkout
// of its latest establishing event, whatever actor recorded it. A second
// actor starting work in the same checkout leaves that checkout holding, and
// any actor's activity there keeps the hold fresh.
func TestTheHolderIsTheCheckoutWhoeverActsInIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []model.IssueEvent
		want   claims.Held
	}{
		{
			name:   "one actor's start holds as its checkout",
			events: []model.IssueEvent{actorEvent("e1", "T1", model.ActionStart, ago(time.Hour), streamA, "claude_a")},
			want:   held(streamA, ago(time.Hour), ago(time.Hour)),
		},
		{
			name: "a second actor's start in the same checkout holds as the same checkout",
			events: []model.IssueEvent{
				actorEvent("e1", "T1", model.ActionStart, ago(2*time.Hour), streamA, "claude_a"),
				actorEvent("e2", "T2", model.ActionStart, ago(time.Hour), streamA, "claude_b"),
			},
			want: held(streamA, ago(time.Hour), ago(time.Hour)),
		},
		{
			name: "another actor's comment in the checkout keeps its hold fresh",
			events: []model.IssueEvent{
				actorEvent("e1", "T1", model.ActionStart, ago(3*time.Hour), streamA, "claude_a"),
				actorEvent("e2", "T1", "", ago(time.Hour), streamA, "claude_b"),
			},
			want: held(streamA, ago(3*time.Hour), ago(time.Hour)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues, parents := epicOf(t, leaf(t, "T1", "", model.StateInProgress), leaf(t, "T2", "", model.StateOpen))
			standings := derive(t, issues, parents, tc.events, bothLive)
			assertStanding(t, standings.Of(laneIn(epicID, "")), tc.want)
		})
	}
}
