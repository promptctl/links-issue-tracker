package claims_test

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// sessionEvent is event with the actor the store stamps: the session that
// recorded it.
func sessionEvent(id, issueID model.ID, action model.ActionName, at time.Time, by model.Attribution, actor model.Text) model.IssueEvent {
	e := event(id, issueID, action, at, by)
	e.Actor = actor
	return e
}

// TestHeldNamesCheckoutAndSession: the holder a lane derives is the checkout
// AND the session of its latest establishing event. A second session that
// starts work in the same checkout takes the lane like any rival and leaves
// the first as its contestant, because one checkout is not one holder. An
// actor that names nobody — the store's "unknown" — derives the zero session,
// the same holder an empty actor derives.
func TestHeldNamesCheckoutAndSession(t *testing.T) {
	sessionA := model.SessionOf("claude_a")
	sessionB := model.SessionOf("claude_b")
	for _, tc := range []struct {
		name   string
		events []model.IssueEvent
		want   claims.Held
	}{
		{
			name:   "one session's start holds as that session in that checkout",
			events: []model.IssueEvent{sessionEvent("e1", "T1", model.ActionStart, ago(time.Hour), streamA, "claude_a")},
			want:   claims.Held{Tenure: claims.Tenure{By: claims.Holder{Checkout: streamA, Session: sessionA}, Since: ago(time.Hour), LastActivity: ago(time.Hour)}},
		},
		{
			name: "a second session establishing in the same checkout holds, contested by the first",
			events: []model.IssueEvent{
				sessionEvent("e1", "T1", model.ActionStart, ago(2*time.Hour), streamA, "claude_a"),
				sessionEvent("e2", "T2", model.ActionStart, ago(time.Hour), streamA, "claude_b"),
			},
			want: claims.Held{
				Tenure:    claims.Tenure{By: claims.Holder{Checkout: streamA, Session: sessionB}, Since: ago(time.Hour), LastActivity: ago(time.Hour)},
				Contested: []claims.Holder{{Checkout: streamA, Session: sessionA}},
			},
		},
		{
			name: "another session's comment keeps only its own trail, not the holder's",
			events: []model.IssueEvent{
				sessionEvent("e1", "T1", model.ActionStart, ago(3*time.Hour), streamA, "claude_a"),
				sessionEvent("e2", "T1", "", ago(time.Hour), streamA, "claude_b"),
			},
			want: claims.Held{Tenure: claims.Tenure{By: claims.Holder{Checkout: streamA, Session: sessionA}, Since: ago(3 * time.Hour), LastActivity: ago(3 * time.Hour)}},
		},
		{
			name:   "an unknown actor holds as the zero session",
			events: []model.IssueEvent{sessionEvent("e1", "T1", model.ActionStart, ago(time.Hour), streamA, "unknown")},
			want:   claims.Held{Tenure: claims.Tenure{By: claims.Holder{Checkout: streamA}, Since: ago(time.Hour), LastActivity: ago(time.Hour)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues, parents := epicOf(t, leaf(t, "T1", "", model.StateInProgress), leaf(t, "T2", "", model.StateOpen))
			standings := derive(t, issues, parents, tc.events, bothLive)
			assertStanding(t, standings.Of(laneIn(epicID, "")), tc.want)
		})
	}
}
