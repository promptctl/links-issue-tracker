package model

import (
	"slices"
	"testing"
	"time"
)

// TestApplyPurgesDropsEveryRowOfAPurgedIssueAndNothingElse pins the cutter on
// its own, because the merge also scopes edges to surviving issues and would
// hide a cutter that kept one: a restore has no such second filter. An edge
// goes if EITHER end is purged, so both directions are fixtures.
func TestApplyPurgesDropsEveryRowOfAPurgedIssueAndNothingElse(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	in := Export{
		Issues: []Issue{{ID: "gone", CreatedAt: now}, {ID: "kept", CreatedAt: now, FollowsUp: "gone"}},
		Relations: []Relation{
			{SrcID: "gone", DstID: "kept", Type: RelBlockedBy},
			{SrcID: "kept", DstID: "gone", Type: RelRelatedTo},
		},
		Comments: []Comment{{ID: "c-gone", IssueID: "gone"}, {ID: "c-kept", IssueID: "kept", Body: "see gone"}},
		Labels:   []Label{{IssueID: "gone", Name: "a"}, {IssueID: "kept", Name: "b"}},
		Events:   []IssueEvent{{ID: "e-gone", IssueID: "gone"}, {ID: "e-kept", IssueID: "kept"}},
	}
	purges := []Purge{{IssueID: "gone", IssueCreatedAt: now, PurgedAt: now, PurgedBy: "grace"}}

	out := in.ApplyPurges(purges)

	if len(out.Issues) != 1 || out.Issues[0].ID != "kept" || out.Issues[0].FollowsUp != "gone" {
		t.Errorf("issues = %+v, want kept alone with its follows_up intact", out.Issues)
	}
	if len(out.Relations) != 0 {
		t.Errorf("relations = %+v, want none: each has the purged issue at one end", out.Relations)
	}
	if len(out.Comments) != 1 || out.Comments[0].ID != "c-kept" || out.Comments[0].Body != "see gone" {
		t.Errorf("comments = %+v, want c-kept with its text intact", out.Comments)
	}
	if len(out.Labels) != 1 || out.Labels[0].IssueID != "kept" || len(out.Events) != 1 || out.Events[0].ID != "e-kept" {
		t.Errorf("labels = %+v events = %+v, want kept's alone", out.Labels, out.Events)
	}
	if len(out.Purges) != 1 || out.Purges[0] != purges[0] {
		t.Errorf("purges = %+v, want %+v", out.Purges, purges)
	}
	if len(in.Issues) != 2 || len(in.Relations) != 2 || len(in.Comments) != 2 {
		t.Errorf("ApplyPurges changed the export it was handed: %+v", in)
	}
}

// TestApplyPurgesLeavesAnotherTicketUnderAPurgedID pins identity: a record
// names a ticket by its id and its birth, so a different ticket wearing the
// same id — an unrelated store's coincidence — keeps every row. The same birth
// spelled in another offset is still the purged ticket.
func TestApplyPurgesLeavesAnotherTicketUnderAPurgedID(t *testing.T) {
	born := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	purges := []Purge{{IssueID: "x", IssueCreatedAt: born, PurgedAt: born, PurgedBy: "grace"}}
	stranger := Export{
		Issues:   []Issue{{ID: "x", CreatedAt: born.Add(time.Second)}},
		Comments: []Comment{{ID: "c", IssueID: "x"}},
	}
	if out := stranger.ApplyPurges(purges); len(out.Issues) != 1 || len(out.Comments) != 1 {
		t.Errorf("ApplyPurges cut a different ticket under the purged id: %+v", out)
	}
	replica := Export{Issues: []Issue{{ID: "x", CreatedAt: born.In(time.FixedZone("east", 3600))}}}
	if out := replica.ApplyPurges(purges); len(out.Issues) != 0 {
		t.Errorf("ApplyPurges kept the purged ticket spelled in another offset: %+v", out.Issues)
	}
}

// TestUnionPurgesKeepsTheEarliestRecordOfEachTicket pins convergence: two
// stores that purged one ticket independently, read in either order, keep one
// record — the earlier one, the actor breaking a tie. Two tickets under one id
// are two records, ordered by birth.
func TestUnionPurgesKeepsTheEarliestRecordOfEachTicket(t *testing.T) {
	early := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	a := []Purge{{IssueID: "z", PurgedAt: late, PurgedBy: "ada"}, {IssueID: "x", IssueCreatedAt: late, PurgedAt: early, PurgedBy: "zed"}}
	b := []Purge{{IssueID: "z", PurgedAt: early, PurgedBy: "grace"}, {IssueID: "x", IssueCreatedAt: late, PurgedAt: early, PurgedBy: "ada"}, {IssueID: "x", IssueCreatedAt: early, PurgedAt: late, PurgedBy: "bo"}}
	want := []Purge{{IssueID: "x", IssueCreatedAt: early, PurgedAt: late, PurgedBy: "bo"}, {IssueID: "x", IssueCreatedAt: late, PurgedAt: early, PurgedBy: "ada"}, {IssueID: "z", PurgedAt: early, PurgedBy: "grace"}}

	for _, got := range [][]Purge{UnionPurges(a, b), UnionPurges(b, a)} {
		if !slices.Equal(got, want) {
			t.Errorf("UnionPurges = %+v, want %+v", got, want)
		}
	}
}
