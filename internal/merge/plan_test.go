package merge

import (
	"slices"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// A plan is prose: two sides rewriting it at once are held for a reader, never
// picked. Plan reviews are written once and never edited, so a merge keeps
// every review either side holds, once — a review recorded on one machine is
// still recorded after a sync with another.
func TestThreeWayHoldsAConcurrentPlanAndKeepsEveryPlanReview(t *testing.T) {
	now := time.Now().UTC()
	issue := issueWithStatus(t, model.Issue{ID: "i1", Title: "issue", IssueType: "task", Plan: "add a retry", CreatedAt: now, UpdatedAt: now}, model.StateOpen)
	review := func(id string) model.PlanReview {
		return model.PlanReview{ID: id, IssueID: "i1", PlanDigest: model.DigestPlan("add a retry"), Verdict: model.PlanPass, Laws: []model.Law{}, CreatedAt: now, CreatedBy: "grace"}
	}
	shared := review("rvw-shared")
	base := model.Export{Issues: []model.Issue{issue}, PlanReviews: []model.PlanReview{shared}}
	local := model.Export{Issues: []model.Issue{issue}, PlanReviews: []model.PlanReview{shared, review("rvw-local")}}
	remote := model.Export{Issues: []model.Issue{issue}, PlanReviews: []model.PlanReview{shared, review("rvw-remote")}}
	local.Issues[0].Plan = "drop the retry"
	remote.Issues[0].Plan = "bound the retry"

	result := ThreeWay(base, local, remote)
	if len(result.Pending) != 1 {
		t.Fatalf("pending = %#v, want the plan held", result.Pending)
	}
	if held := result.Pending[0]; held.IssueID != "i1" || held.Field != ProsePlan || held.Base != "add a retry" || held.Ours != "drop the retry" || held.Theirs != "bound the retry" {
		t.Errorf("held prose = %#v, want i1's plan with every side's text", held)
	}
	var ids []string
	for _, r := range provisional(t, result).PlanReviews {
		ids = append(ids, r.ID)
	}
	if want := []string{"rvw-local", "rvw-remote", "rvw-shared"}; !slices.Equal(ids, want) {
		t.Errorf("merged plan reviews = %v, want %v", ids, want)
	}
}
