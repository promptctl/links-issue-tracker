package memory

import (
	"context"

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
	return review, nil
}

func (e *Engine) planReviewsFor(issueID string) []model.PlanReview {
	out := []model.PlanReview{}
	for _, review := range e.planReviews {
		if review.IssueID == issueID {
			out = append(out, review)
		}
	}
	return out
}
