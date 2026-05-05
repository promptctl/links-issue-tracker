package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/issueid"
)

func newIssueID(ctx context.Context, tx *sql.Tx, prefix string, topic string, title string, description string, createdBy string, createdAt time.Time, parentID string) (string, error) {
	if strings.TrimSpace(parentID) != "" {
		return newChildIssueID(ctx, tx, parentID)
	}
	return newTopLevelIssueID(ctx, tx, prefix, topic, title, description, createdBy, createdAt)
}

func newTopLevelIssueID(ctx context.Context, tx *sql.Tx, prefix string, topic string, title string, description string, createdBy string, createdAt time.Time) (string, error) {
	baseLength, err := getAdaptiveIssueIDLength(ctx, tx)
	if err != nil {
		baseLength = 6
	}
	if baseLength > issueid.MaxHashLength {
		baseLength = issueid.MaxHashLength
	}
	for length := baseLength; length <= issueid.MaxHashLength; length++ {
		for nonce := 0; nonce < issueid.NonceAttempts; nonce++ {
			candidate := issueid.GenerateHashID(prefix, topic, title, description, createdBy, createdAt, length, nonce)
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE id = ?`, candidate).Scan(&count); err != nil {
				return "", fmt.Errorf("check issue id collision: %w", err)
			}
			if count == 0 {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("generate unique issue id: exhausted lengths %d-%d", baseLength, issueid.MaxHashLength)
}

func newChildIssueID(ctx context.Context, tx *sql.Tx, parentID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM issues WHERE id LIKE ?`, parentID+".%")
	if err != nil {
		return "", fmt.Errorf("query child ids: %w", err)
	}
	defer rows.Close()

	maxChildNumber := 0
	for rows.Next() {
		var candidate string
		if err := rows.Scan(&candidate); err != nil {
			return "", fmt.Errorf("scan child id: %w", err)
		}
		suffix := strings.TrimPrefix(candidate, parentID+".")
		if suffix == "" || strings.Contains(suffix, ".") {
			continue
		}
		childNumber, err := strconv.Atoi(suffix)
		if err != nil {
			continue
		}
		if childNumber > maxChildNumber {
			maxChildNumber = childNumber
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate child ids: %w", err)
	}
	return fmt.Sprintf("%s.%d", parentID, maxChildNumber+1), nil
}

// [LAW:dataflow-not-control-flow] Adaptive length is a pure function of the
// top-level issue population. The prefix never gates the count: every issue in
// a workspace shares one generation-time prefix, and even after a rename the
// collision space we care about is "all top-level IDs in this DB" — counting
// across prefixes is conservative (slightly longer hashes) and never wrong.
func getAdaptiveIssueIDLength(ctx context.Context, tx *sql.Tx) (int, error) {
	numIssues, err := countTopLevelIssues(ctx, tx)
	if err != nil {
		return 6, err
	}
	return issueid.ComputeAdaptiveLength(numIssues), nil
}

func countTopLevelIssues(ctx context.Context, tx *sql.Tx) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE id NOT LIKE ?`, "%.%").Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
