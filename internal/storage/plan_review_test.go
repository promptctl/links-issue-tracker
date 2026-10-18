package storage_test

import (
	"errors"
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
		{name: "the latest of several passes", plan: first, events: authored,
			reviews:  []model.PlanReview{review("r1", "ada", first, model.PlanPass), review("r2", "author", first, model.PlanPass), review("r3", "grace", first, model.PlanPass)},
			approved: "r3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := model.Issue{ID: "lit-1", Plan: tc.plan}
			got, err := storage.PlanApproval(issue, tc.events, tc.reviews)
			if tc.gap == nil {
				if err != nil || got.ID != tc.approved {
					t.Fatalf("PlanApproval = %s, %v; want approved on %s", got.ID, err, tc.approved)
				}
				return
			}
			var unapproved storage.PlanUnapprovedError
			if !errors.As(err, &unapproved) || unapproved.IssueID != "lit-1" || !reflect.DeepEqual(unapproved.Gap, tc.gap) {
				t.Fatalf("PlanApproval = %s, %#v; want PlanUnapprovedError with gap %#v", got.ID, err, tc.gap)
			}
		})
	}
}

// The refusal says what is missing, in the words lit show uses for the plan's
// version and digest, and a fail names who gave it and the laws it cites.
func TestPlanUnapprovedErrorSaysWhatIsMissing(t *testing.T) {
	version := storage.PlanVersion{Number: 2, Digest: model.DigestPlan("plan")}
	fail := model.PlanReview{CreatedBy: "ada", Verdict: model.PlanFail, Laws: []model.Law{"carrying-cost", "single-enforcer"}}
	for _, tc := range []struct {
		gap  storage.PlanGap
		kind string
		want string
	}{
		{storage.PlanMissing{}, "no_plan",
			"lit-1 has no plan; a ticket is finished once a reviewer other than its plan's author has passed the current plan"},
		{storage.PlanUnreviewed{Plan: version}, "unreviewed",
			"lit-1's current plan (v2, digest " + string(version.Digest) + ") has no review"},
		{storage.PlanUnreviewed{Plan: version, EarlierReviewed: true}, "unreviewed",
			"lit-1's current plan (v2, digest " + string(version.Digest) + ") has no review; the reviews on record judged an earlier text"},
		{storage.PlanFailed{Plan: version, Fails: []model.PlanReview{fail}}, "failed",
			"lit-1's current plan (v2, digest " + string(version.Digest) + ") failed its law review: ada failed it citing carrying-cost, single-enforcer; a fail stands until its reviewer passes the same text, so revise the plan and have the new text reviewed"},
		{storage.PlanSelfReviewed{Plan: version, Author: "author"}, "author_only",
			"lit-1's current plan (v2, digest " + string(version.Digest) + ") has passed only its author's own review (author); it needs a pass from a reviewer other than its author"},
	} {
		err := storage.PlanUnapprovedError{IssueID: "lit-1", Gap: tc.gap}
		if err.Error() != tc.want || tc.gap.Kind() != tc.kind {
			t.Errorf("%T: Error() = %q, Kind() = %q\nwant %q, %q", tc.gap, err.Error(), tc.gap.Kind(), tc.want, tc.kind)
		}
	}
}
