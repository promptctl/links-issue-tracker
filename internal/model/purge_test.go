package model

import (
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
		Issues: []Issue{{ID: "gone"}, {ID: "kept", FollowsUp: "gone"}},
		Relations: []Relation{
			{SrcID: "gone", DstID: "kept", Type: RelBlockedBy},
			{SrcID: "kept", DstID: "gone", Type: RelRelatedTo},
		},
		Comments: []Comment{{ID: "c-gone", IssueID: "gone"}, {ID: "c-kept", IssueID: "kept", Body: "see gone"}},
		Labels:   []Label{{IssueID: "gone", Name: "a"}, {IssueID: "kept", Name: "b"}},
		Events:   []IssueEvent{{ID: "e-gone", IssueID: "gone"}, {ID: "e-kept", IssueID: "kept"}},
	}
	purges := []Purge{{IssueID: "gone", PurgedAt: now, PurgedBy: "grace"}}

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

// TestUnionPurgesKeepsTheEarliestRecordOfEachID pins convergence: two stores
// that purged one id independently, read in either order, keep one record —
// the earlier one, the actor breaking a tie — and the union is by id.
func TestUnionPurgesKeepsTheEarliestRecordOfEachID(t *testing.T) {
	early := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	a := []Purge{{IssueID: "z", PurgedAt: late, PurgedBy: "ada"}, {IssueID: "x", PurgedAt: early, PurgedBy: "zed"}}
	b := []Purge{{IssueID: "z", PurgedAt: early, PurgedBy: "grace"}, {IssueID: "x", PurgedAt: early, PurgedBy: "ada"}}
	want := []Purge{{IssueID: "x", PurgedAt: early, PurgedBy: "ada"}, {IssueID: "z", PurgedAt: early, PurgedBy: "grace"}}

	for _, got := range [][]Purge{UnionPurges(a, b), UnionPurges(b, a)} {
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("UnionPurges = %+v, want %+v", got, want)
		}
	}
}
