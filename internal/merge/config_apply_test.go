package merge

import (
	"slices"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Config applies are never edited or removed and belong to no issue: a merge
// keeps every apply either side holds, once, so both stores converge on one
// set and agree on the newest, the config in force.
func TestThreeWayKeepsEveryConfigApply(t *testing.T) {
	at := func(seconds int) time.Time { return time.Date(2026, 1, 1, 0, 0, seconds, 0, time.UTC) }
	shared := model.ConfigApply{ID: "cfg-shared", Content: "a", CreatedAt: at(1), CreatedBy: "ada"}
	local := model.ConfigApply{ID: "cfg-local", Content: "b", CreatedAt: at(2), CreatedBy: "ada"}
	remote := model.ConfigApply{ID: "cfg-remote", Content: "c", CreatedAt: at(3), CreatedBy: "grace"}
	base := model.Export{ConfigApplies: []model.ConfigApply{shared}}
	ours := model.Export{ConfigApplies: []model.ConfigApply{shared, local}}
	theirs := model.Export{ConfigApplies: []model.ConfigApply{shared, remote}, Purges: []model.Purge{{IssueID: "gone", IssueCreatedAt: at(0), PurgedAt: at(4), PurgedBy: "grace"}}}

	for _, merged := range []model.Export{provisional(t, ThreeWay(base, ours, theirs)), provisional(t, ThreeWay(base, theirs, ours))} {
		var ids []model.ID
		for _, apply := range merged.ConfigApplies {
			ids = append(ids, apply.ID)
		}
		if want := []model.ID{"cfg-shared", "cfg-local", "cfg-remote"}; !slices.Equal(ids, want) {
			t.Errorf("merged applies = %v, want %v, oldest first", ids, want)
		}
		if inForce, _ := model.ConfigInForce(merged.ConfigApplies); inForce.ID != remote.ID {
			t.Errorf("in force after the merge = %s, want the newest apply", inForce.ID)
		}
	}
}
