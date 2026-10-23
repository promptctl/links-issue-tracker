package claims_test

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// TestLaneProgressCountsAndNamesTheActiveMember exercises LaneProgress
// against an epic-major lane with a closed member, an in-progress member,
// and an open one — the shape the dossier's "5/9 done" line and its named
// active ticket both come from.
func TestLaneProgressCountsAndNamesTheActiveMember(t *testing.T) {
	children := []model.Issue{
		leaf(t, "T1", "lane", model.StateClosed),
		leaf(t, "T2", "lane", model.StateInProgress),
		leaf(t, "T3", "lane", model.StateOpen),
	}
	issues, parents := epicOf(t, children...)
	evidence, err := claims.NewEvidence(issues, parents, nil)
	if err != nil {
		t.Fatalf("NewEvidence() error = %v", err)
	}

	progress := evidence.LaneProgress(laneIn(epicID, "lane"))
	if progress.Done != 1 || progress.Total != 3 {
		t.Fatalf("LaneProgress = {Done:%d Total:%d}, want {Done:1 Total:3}", progress.Done, progress.Total)
	}
	if progress.Active == nil || progress.Active.ID != "T2" {
		t.Fatalf("LaneProgress.Active = %v, want T2", progress.Active)
	}
}

// TestLaneProgressWithNoInProgressMemberNamesNoActiveTicket covers the
// common case — a lane whose evidence carries no in_progress member at all,
// which formatLaneProgress in the cli package reads as "done/total" with no
// ticket named.
func TestLaneProgressWithNoInProgressMemberNamesNoActiveTicket(t *testing.T) {
	children := []model.Issue{
		leaf(t, "T1", "lane", model.StateClosed),
		leaf(t, "T2", "lane", model.StateOpen),
	}
	issues, parents := epicOf(t, children...)
	evidence, err := claims.NewEvidence(issues, parents, nil)
	if err != nil {
		t.Fatalf("NewEvidence() error = %v", err)
	}

	progress := evidence.LaneProgress(laneIn(epicID, "lane"))
	if progress.Done != 1 || progress.Total != 2 {
		t.Fatalf("LaneProgress = {Done:%d Total:%d}, want {Done:1 Total:2}", progress.Done, progress.Total)
	}
	if progress.Active != nil {
		t.Fatalf("LaneProgress.Active = %v, want nil", progress.Active)
	}
}

// LaneProgressWith counts the written ticket in place of the version the
// evidence read, so a command that wrote after gathering learns the lane as it
// now stands — and the evidence it asked still reads as it did.
func TestLaneProgressWithCountsTheWrittenTicketInPlaceOfTheOneRead(t *testing.T) {
	children := []model.Issue{
		leaf(t, "T1", "lane", model.StateClosed),
		leaf(t, "T2", "lane", model.StateOpen),
	}
	issues, parents := epicOf(t, children...)
	evidence, err := claims.NewEvidence(issues, parents, nil)
	if err != nil {
		t.Fatalf("NewEvidence() error = %v", err)
	}
	lane := laneIn(epicID, "lane")
	cases := []struct {
		name        string
		written     model.Issue
		done, total int
	}{
		{"starting an open ticket leaves the done count", leaf(t, "T2", "lane", model.StateInProgress), 1, 2},
		{"starting a closed ticket takes it out of the done count", leaf(t, "T1", "lane", model.StateInProgress), 0, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			progress := evidence.LaneProgressWith(tc.written)
			if progress.Done != tc.done || progress.Total != tc.total {
				t.Fatalf("LaneProgressWith = {Done:%d Total:%d}, want {Done:%d Total:%d}", progress.Done, progress.Total, tc.done, tc.total)
			}
			if progress.Active == nil || progress.Active.ID != tc.written.ID {
				t.Fatalf("LaneProgressWith.Active = %v, want %s", progress.Active, tc.written.ID)
			}
			if read := evidence.LaneProgress(lane); read.Done != 1 || read.Total != 2 || read.Active != nil {
				t.Fatalf("LaneProgress after LaneProgressWith = %+v, want the evidence as read: {Done:1 Total:2}, nothing active", read)
			}
		})
	}
}

// An archived or deleted ticket has left the lane's work, closed or not, so it
// is neither done nor outstanding: a deleted open ticket would otherwise hold
// the lane short of finished for good. The same tickets live count, which is
// what makes the frozen ones' absence the rule rather than a miscount.
func TestLaneProgressCountsOnlyMembersInTheFlow(t *testing.T) {
	frozen := func(issue model.Issue, retention model.Retention) model.Issue {
		issue.SetRetention(retention)
		return issue
	}
	live := []model.Issue{
		leaf(t, "T1", "lane", model.StateClosed),
		leaf(t, "T2", "lane", model.StateOpen),
		leaf(t, "T3", "lane", model.StateOpen),
		leaf(t, "T4", "lane", model.StateClosed),
		leaf(t, "T5", "lane", model.StateInProgress),
	}
	cases := []struct {
		name        string
		children    []model.Issue
		done, total int
		active      bool
	}{
		{"all live", live, 2, 5, true},
		{"archived and deleted members left out", []model.Issue{
			live[0],
			live[1],
			frozen(live[2], model.Deleted{}),
			frozen(live[3], model.Archived{}),
			frozen(live[4], model.Archived{}),
		}, 1, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues, parents := epicOf(t, tc.children...)
			evidence, err := claims.NewEvidence(issues, parents, nil)
			if err != nil {
				t.Fatalf("NewEvidence() error = %v", err)
			}
			progress := evidence.LaneProgress(laneIn(epicID, "lane"))
			if progress.Done != tc.done || progress.Total != tc.total || (progress.Active != nil) != tc.active {
				t.Fatalf("LaneProgress = %+v, want {Done:%d Total:%d}, active member %t", progress, tc.done, tc.total, tc.active)
			}
		})
	}
}

// TestLaneProgressOfUnseenLaneIsZero mirrors Standings.Of's total-map-read
// convention: a lane the evidence never partitioned reports the zero value
// rather than panicking on a missing map entry.
func TestLaneProgressOfUnseenLaneIsZero(t *testing.T) {
	evidence, err := claims.NewEvidence(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewEvidence() error = %v", err)
	}
	progress := evidence.LaneProgress(soloLane("ghost"))
	if progress.Total != 0 || progress.Done != 0 || progress.Active != nil {
		t.Fatalf("LaneProgress(unseen) = %+v, want zero value", progress)
	}
}

// A scope's progress counts every lane the scope admits as one set of tickets
// and nothing outside it: here the two lanes of epic E, and not epic F, whose
// closed ticket would otherwise read as E's. A frozen member of an admitted
// lane has left the work there as it has in a lane's own count.
func TestScopeProgressCountsEveryAdmittedLaneAndNothingElse(t *testing.T) {
	archived := leaf(t, "E3", "b", model.StateClosed)
	archived.SetRetention(model.Archived{})
	issues, parents := epicOf(t,
		leaf(t, "E1", "a", model.StateClosed),
		leaf(t, "E2", "a", model.StateOpen),
		archived,
		leaf(t, "E4", "b", model.StateClosed),
		leaf(t, "E5", "b", model.StateInProgress),
	)
	other := leaf(t, "F1", "", model.StateClosed)
	epicF := container(t, "F", other)
	issues = append(issues, epicF, other)
	parents[other.ID] = epicF.ID
	evidence, err := claims.NewEvidence(issues, parents, nil)
	if err != nil {
		t.Fatalf("NewEvidence() error = %v", err)
	}

	for _, tc := range []struct {
		name    string
		inScope func(model.LaneID) bool
		want    claims.Progress
	}{
		{"both lanes of E", func(lane model.LaneID) bool { return lane.Epic() == epicID }, claims.Progress{Done: 2, Total: 4}},
		{"one lane of E", func(lane model.LaneID) bool { return lane == laneIn(epicID, "b") }, claims.Progress{Done: 1, Total: 2}},
		{"no lane", func(model.LaneID) bool { return false }, claims.Progress{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evidence.ScopeProgress(tc.inScope); got != tc.want {
				t.Fatalf("ScopeProgress = %+v, want %+v", got, tc.want)
			}
		})
	}
}
