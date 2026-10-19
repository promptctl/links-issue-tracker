package storage_test

import (
	"reflect"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A ticket's current plan is approved when each reviewer's latest verdict on
// that text leaves no fail standing and at least one standing pass is by
// someone other than the text's author: the actor of the latest event that
// wrote the plan to it, or the issue's creator for a plan given at creation.
func TestPlanApprovalNamesWhatTheCurrentPlanLacks(t *testing.T) {
	const first, second = "Add a retry around the push", "Drop the retry; the lease already bounds it"
	created := model.IssueEvent{Action: model.ActionCreated, Actor: "creator"}
	wrote := func(actor, from, to string) model.IssueEvent {
		return model.IssueEvent{Action: "update", Actor: actor, Changes: []model.FieldChange{{Field: "plan", From: from, To: to}}}
	}
	review := func(id, by, text string, verdict model.PlanVerdict, laws ...model.Law) model.PlanReview {
		return model.PlanReview{ID: id, IssueID: "lit-1", PlanDigest: model.DigestPlan(text), Verdict: verdict, Laws: laws, CreatedBy: by}
	}
	v1 := storage.PlanVersion{Number: 1, Digest: model.DigestPlan(first)}
	v2 := storage.PlanVersion{Number: 2, Digest: model.DigestPlan(second)}
	authored := []model.IssueEvent{created, wrote("author", "", first)}
	revised := []model.IssueEvent{created, wrote("author", "", first), wrote("author", first, second)}
	reverted := []model.IssueEvent{created, wrote("author", "", first), wrote("author", first, second), wrote("author", second, first)}
	revertedByReviewer := []model.IssueEvent{created, wrote("author", "", first), wrote("author", first, second), wrote("ada", second, first)}
	// Two stores edited the plan at once and the merge kept the earlier edit:
	// the later event wrote a text that is not the current one.
	mergedConcurrent := []model.IssueEvent{created, wrote("author", "", first), wrote("ada", "", second)}

	for _, tc := range []struct {
		name     string
		plan     string
		events   []model.IssueEvent
		reviews  []model.PlanReview
		approved string
		gap      storage.PlanGap
	}{
		{name: "no plan", events: []model.IssueEvent{created}, gap: storage.PlanMissing{}},
		{name: "never reviewed", plan: first, events: authored, gap: storage.PlanUnreviewed{Plan: v1}},
		{name: "only an earlier text reviewed", plan: second, events: revised,
			reviews: []model.PlanReview{review("r1", "ada", first, model.PlanPass)},
			gap:     storage.PlanUnreviewed{Plan: v2, EarlierReviewed: true}},
		{name: "a pass by someone else", plan: first, events: authored,
			reviews:  []model.PlanReview{review("r1", "ada", first, model.PlanPass)},
			approved: "r1"},
		{name: "only the author's own pass", plan: first, events: authored,
			reviews: []model.PlanReview{review("r1", "author", first, model.PlanPass)},
			gap:     storage.PlanSelfReviewed{Plan: v1, Author: "author"}},
		{name: "a plan given at creation, passed by its creator", plan: first, events: []model.IssueEvent{created},
			reviews: []model.PlanReview{review("r1", "creator", first, model.PlanPass)},
			gap:     storage.PlanSelfReviewed{Plan: v1, Author: "creator"}},
		{name: "a plan given at creation, passed by someone else", plan: first, events: []model.IssueEvent{created},
			reviews:  []model.PlanReview{review("r1", "ada", first, model.PlanPass)},
			approved: "r1"},
		{name: "a fail, then a fresh reviewer's pass of the same text", plan: first, events: authored,
			reviews: []model.PlanReview{review("r1", "ada", first, model.PlanFail, "carrying-cost"), review("r2", "grace", first, model.PlanPass)},
			gap:     storage.PlanFailed{Plan: v1, Fails: []model.PlanReview{review("r1", "ada", first, model.PlanFail, "carrying-cost")}}},
		{name: "a reviewer reversing their own fail", plan: first, events: authored,
			reviews:  []model.PlanReview{review("r1", "ada", first, model.PlanFail, "carrying-cost"), review("r2", "ada", first, model.PlanPass)},
			approved: "r2"},
		{name: "a reviewer reversing their own pass", plan: first, events: authored,
			reviews: []model.PlanReview{review("r1", "ada", first, model.PlanPass), review("r2", "ada", first, model.PlanFail, "single-enforcer")},
			gap:     storage.PlanFailed{Plan: v1, Fails: []model.PlanReview{review("r2", "ada", first, model.PlanFail, "single-enforcer")}}},
		{name: "standing fails, newest first", plan: first, events: authored,
			reviews: []model.PlanReview{review("r1", "ada", first, model.PlanFail, "carrying-cost"), review("r2", "grace", first, model.PlanFail, "single-enforcer")},
			gap:     storage.PlanFailed{Plan: v1, Fails: []model.PlanReview{review("r2", "grace", first, model.PlanFail, "single-enforcer"), review("r1", "ada", first, model.PlanFail, "carrying-cost")}}},
		{name: "a fail of an earlier text", plan: second, events: revised,
			reviews:  []model.PlanReview{review("r1", "ada", first, model.PlanFail, "carrying-cost"), review("r2", "ada", second, model.PlanPass)},
			approved: "r2"},
		{name: "a plan returned to a text that passed", plan: first, events: reverted,
			reviews:  []model.PlanReview{review("r1", "ada", first, model.PlanPass)},
			approved: "r1"},
		{name: "a plan returned to a passed text by its reviewer", plan: first, events: revertedByReviewer,
			reviews: []model.PlanReview{review("r1", "ada", first, model.PlanPass)},
			gap:     storage.PlanSelfReviewed{Plan: v1, Author: "ada"}},
		{name: "concurrent edits merged to the earlier text", plan: first, events: mergedConcurrent,
			reviews:  []model.PlanReview{review("r1", "ada", first, model.PlanPass)},
			approved: "r1"},
		{name: "the latest of several passes", plan: first, events: authored,
			reviews:  []model.PlanReview{review("r1", "ada", first, model.PlanPass), review("r2", "author", first, model.PlanPass), review("r3", "grace", first, model.PlanPass)},
			approved: "r3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := model.Issue{ID: "lit-1", Plan: tc.plan}
			got := storage.PlanApproval(issue, tc.events, tc.reviews)
			if tc.gap == nil {
				if approval, ok := got.(storage.PlanApproved); !ok || approval.Review.ID != tc.approved {
					t.Fatalf("PlanApproval = %#v; want approved on %s", got, tc.approved)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.gap) {
				t.Fatalf("PlanApproval = %#v; want gap %#v", got, tc.gap)
			}
		})
	}
}

// A gap says what is missing, in the words lit show uses for the plan's
// version and digest, and a fail names who gave it and the laws it cites.
func TestPlanGapSaysWhatIsMissing(t *testing.T) {
	version := storage.PlanVersion{Number: 2, Digest: model.DigestPlan("plan")}
	fail := model.PlanReview{CreatedBy: "ada", Verdict: model.PlanFail, Laws: []model.Law{"carrying-cost", "single-enforcer"}}
	for _, tc := range []struct {
		gap  storage.PlanGap
		kind string
		want string
	}{
		{storage.PlanMissing{}, "no_plan",
			"lit-1 has no plan"},
		{storage.PlanUnreviewed{Plan: version}, "unreviewed",
			"lit-1's current plan (v2, digest " + string(version.Digest) + ") has no review"},
		{storage.PlanUnreviewed{Plan: version, EarlierReviewed: true}, "unreviewed",
			"lit-1's current plan (v2, digest " + string(version.Digest) + ") has no review; the reviews on record judged an earlier text"},
		{storage.PlanFailed{Plan: version, Fails: []model.PlanReview{fail}}, "failed",
			"lit-1's current plan (v2, digest " + string(version.Digest) + ") failed its law review: ada failed it citing carrying-cost, single-enforcer"},
		{storage.PlanSelfReviewed{Plan: version, Author: "author"}, "author_only",
			"lit-1's current plan (v2, digest " + string(version.Digest) + ") has passed only its author's own review (author)"},
	} {
		if got := tc.gap.Describe("lit-1"); got != tc.want || tc.gap.Kind() != tc.kind {
			t.Errorf("%T: Describe() = %q, Kind() = %q\nwant %q, %q", tc.gap, got, tc.gap.Kind(), tc.want, tc.kind)
		}
	}
}
