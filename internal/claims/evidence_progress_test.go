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
			progress := evidence.LaneProgressWith(lane, tc.written)
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
