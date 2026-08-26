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
