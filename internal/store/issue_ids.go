package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/issueid"
)

// [LAW:verifiable-goals] Remove the startup topic backfill once all pre-topic repositories
// have crossed the sunset window on April 19, 2026.
const legacyTopicMigrationRemoveBy = "2026-04-19"

func (s *Store) EnsureIssuePrefix(ctx context.Context, prefix string) error {
	normalized, err := issueid.NormalizeConfiguredPrefix(prefix)
	if err != nil {
		return fmt.Errorf("normalize issue prefix: %w", err)
	}
	changed, err := s.ensureMetaValue(ctx, "issue_prefix", normalized)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return s.commitWorkingSet(ctx, "set issue prefix")
}

func (s *Store) issuePrefixForTx(ctx context.Context, tx *sql.Tx) (string, error) {
	var prefix string
	if err := tx.QueryRowContext(ctx, `SELECT meta_value FROM meta WHERE meta_key = 'issue_prefix'`).Scan(&prefix); err != nil {
		if err == sql.ErrNoRows {
			return "", errors.New("issue prefix is not configured")
		}
		return "", fmt.Errorf("get issue prefix: %w", err)
	}
	normalized, err := issueid.NormalizeConfiguredPrefix(prefix)
	if err != nil {
		return "", fmt.Errorf("normalize stored issue prefix: %w", err)
	}
	return normalized, nil
}

func newIssueID(ctx context.Context, tx *sql.Tx, prefix string, topic string, title string, description string, createdBy string, createdAt time.Time, parentID string) (string, error) {
	if strings.TrimSpace(parentID) != "" {
		return newChildIssueID(ctx, tx, parentID)
	}
	return newTopLevelIssueID(ctx, tx, prefix, topic, title, description, createdBy, createdAt)
}

func newTopLevelIssueID(ctx context.Context, tx *sql.Tx, prefix string, topic string, title string, description string, createdBy string, createdAt time.Time) (string, error) {
	baseLength, err := getAdaptiveIssueIDLength(ctx, tx, prefix)
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

func getAdaptiveIssueIDLength(ctx context.Context, tx *sql.Tx, prefix string) (int, error) {
	numIssues, err := countTopLevelIssues(ctx, tx, prefix)
	if err != nil {
		return 6, err
	}
	return issueid.ComputeAdaptiveLength(numIssues), nil
}

func countTopLevelIssues(ctx context.Context, tx *sql.Tx, prefix string) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE id LIKE ? AND id NOT LIKE ?`, prefix+"-%", "%.%").Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
