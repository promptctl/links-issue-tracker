package cli

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// TestContestedLanesFiltersAndSorts proves the pure filter behind
// reportContestedLanes: only Held lanes with a live contestant are reported —
// Unclaimed, Stale, and an uncontested Held lane all drop out — and the
// survivors come back in a stable, sorted order.
func TestContestedLanesFiltersAndSorts(t *testing.T) {
	holder := model.NewAttribution("aaaaaaaabbbb", "ws-local")
	contestant := model.NewAttribution("eeeeeeeeffff", "ws-local")

	contestedZ := model.LaneOf(model.Issue{ID: "zzz"}, nil)
	contestedA := model.LaneOf(model.Issue{ID: "aaa"}, nil)
	uncontested := model.LaneOf(model.Issue{ID: "mmm"}, nil)
	stale := model.LaneOf(model.Issue{ID: "sss"}, nil)

	standings := claims.Standings{
		contestedZ:  claims.Held{Tenure: claims.Tenure{By: holder}, Contested: []model.Attribution{contestant}},
		contestedA:  claims.Held{Tenure: claims.Tenure{By: holder}, Contested: []model.Attribution{contestant}},
		uncontested: claims.Held{Tenure: claims.Tenure{By: holder}},
		stale:       claims.Stale{Tenure: claims.Tenure{By: holder}},
	}

	got := contestedLanes(standings)
	if len(got) != 2 {
		t.Fatalf("contestedLanes = %v, want exactly the 2 contested lanes", got)
	}
	if got[0] != contestedA || got[1] != contestedZ {
		t.Fatalf("contestedLanes = %v, want [%s, %s] in sorted order", got, contestedA, contestedZ)
	}
}

// TestContestedLanesEmptyForNoContest proves the negative half directly: no
// contested standing anywhere yields an empty (never nil-panicking) result,
// which is what makes reportContestedLanes print nothing for an ordinary
// reconcile.
func TestContestedLanesEmptyForNoContest(t *testing.T) {
	holder := model.NewAttribution("aaaaaaaabbbb", "ws-local")
	lane := model.LaneOf(model.Issue{ID: "aaa"}, nil)
	standings := claims.Standings{lane: claims.Held{Tenure: claims.Tenure{By: holder}}}

	if got := contestedLanes(standings); len(got) != 0 {
		t.Fatalf("contestedLanes with no contest = %v, want empty", got)
	}
}
