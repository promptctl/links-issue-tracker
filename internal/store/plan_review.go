package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// AddPlanReview records the review storage.PlanReviewOf decides for the issue
// as it stands, with its history, stamped with this store's id and time.
func (s *Store) AddPlanReview(ctx context.Context, in storage.AddPlanReviewInput) (model.PlanReview, error) {
	issue, err := s.GetIssue(ctx, in.IssueID)
	if err != nil {
		return model.PlanReview{}, err
	}
	events, err := s.ListEvents(ctx, issue.ID)
	if err != nil {
		return model.PlanReview{}, err
	}
	review, err := storage.PlanReviewOf(issue, events, in)
	if err != nil {
		return model.PlanReview{}, err
	}
	// Read outside the mutation: the review depends only on the history
	// holding the named text, and a history only gains texts.
	review.ID = "rvw-" + uuid.NewString()
	review.CreatedAt = s.clock.Now()
	if err := s.withMutation(ctx, "add plan review", func(ctx context.Context, tx *sql.Tx) error {
		return insertPlanReviewTx(ctx, tx, review)
	}); err != nil {
		return model.PlanReview{}, err
	}
	return review, nil
}

// ListPlanReviews is every review of issueID's plan, in
// storage.PlanReviewOrdering.
func (s *Store) ListPlanReviews(ctx context.Context, issueID string) ([]model.PlanReview, error) {
	return queryPlanReviews(ctx, s.db, "WHERE issue_id = ?", issueID)
}

// insertPlanReviewTx is the one INSERT into plan_reviews, shared by a new
// review and a restore or replay. [LAW:single-enforcer]
func insertPlanReviewTx(ctx context.Context, tx *sql.Tx, review model.PlanReview) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_reviews(id, issue_id, plan_digest, verdict, laws, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		review.ID, review.IssueID, string(review.PlanDigest), string(review.Verdict), joinLaws(review.Laws), review.CreatedAt.Format(time.RFC3339Nano), review.CreatedBy); err != nil {
		return fmt.Errorf("insert plan review %s: %w", review.ID, err)
	}
	return nil
}

func deletePlanReviewTx(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	return execDelete(ctx, tx, `DELETE FROM plan_reviews WHERE id = ?`, fmt.Sprintf("plan review %s", id), id)
}

// queryPlanReviews reads the plan reviews where selects, in
// storage.PlanReviewOrdering. The order is taken in Go, not in SQL: created_at
// is a varchar, and ORDER BY on it sorts a stamp by its spelling.
func queryPlanReviews(ctx context.Context, q rowQueryer, where string, args ...any) ([]model.PlanReview, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, issue_id, plan_digest, verdict, laws, created_at, created_by FROM plan_reviews `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list plan reviews: %w", err)
	}
	defer rows.Close()
	out := []model.PlanReview{}
	for rows.Next() {
		var review model.PlanReview
		var digest, verdict, laws, createdAt string
		if err := rows.Scan(&review.ID, &review.IssueID, &digest, &verdict, &laws, &createdAt, &review.CreatedBy); err != nil {
			return nil, err
		}
		review.PlanDigest = model.PlanDigest(digest)
		review.Verdict = model.PlanVerdict(verdict)
		review.Laws = splitLaws(laws)
		if review.CreatedAt, err = scanTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, review)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, storage.PlanReviewOrdering)
	return out, nil
}

// joinLaws and splitLaws are the laws column's encoding: the tokens joined by
// commas, which model.ParseLaw keeps out of every token; no laws is the empty
// text.
func joinLaws(laws []model.Law) string {
	tokens := make([]string, len(laws))
	for i, law := range laws {
		tokens[i] = string(law)
	}
	return strings.Join(tokens, ",")
}

func splitLaws(column string) []model.Law {
	laws := []model.Law{}
	for _, token := range strings.Split(column, ",") {
		if token != "" {
			laws = append(laws, model.Law(token))
		}
	}
	return laws
}
