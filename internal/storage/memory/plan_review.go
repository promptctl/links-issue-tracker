package memory

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// AddPlanReview records the review storage.PlanReviewOf decides for the issue
// as it stands, with its history, stamped with this engine's id and time.
func (e *Engine) AddPlanReview(ctx context.Context, in storage.AddPlanReviewInput) (model.PlanReview, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	issue, err := e.getIssue(in.IssueID)
	if err != nil {
		return model.PlanReview{}, err
	}
	review, err := storage.PlanReviewOf(issue, e.eventsFor(issue.ID), in)
	if err != nil {
		return model.PlanReview{}, err
	}
	review.ID = "rvw-" + uuid.NewString()
	review.CreatedAt = e.clock.Now()
	e.planReviews = append(e.planReviews, review)
	return clonePlanReviews([]model.PlanReview{review})[0], nil
}

func (e *Engine) planReviewsFor(issueID string) []model.PlanReview {
	return clonePlanReviews(slices.DeleteFunc(slices.Clone(e.planReviews), func(review model.PlanReview) bool { return review.IssueID != issueID }))
}

// clonePlanReviews copies plan reviews for a caller to keep, laws and all, in
// storage.PlanReviewOrdering.
func clonePlanReviews(reviews []model.PlanReview) []model.PlanReview {
	out := make([]model.PlanReview, len(reviews))
	for i, review := range reviews {
		review.Laws = slices.Clone(review.Laws)
		out[i] = review
	}
	slices.SortFunc(out, storage.PlanReviewOrdering)
	return out
}
