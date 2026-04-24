package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type AddLabelInput struct {
	IssueID   string
	Name      string
	CreatedBy string
}

func (s *Store) AddLabel(ctx context.Context, in AddLabelInput) ([]string, error) {
	if _, err := s.GetIssue(ctx, in.IssueID); err != nil {
		return nil, err
	}
	label, err := normalizeLabel(in.Name)
	if err != nil {
		return nil, err
	}
	createdBy := strings.TrimSpace(in.CreatedBy)
	if createdBy == "" {
		createdBy = "unknown"
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin add label tx: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO labels(issue_id, label, created_at, created_by)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE issue_id = issue_id`, in.IssueID, label, time.Now().UTC().Format(time.RFC3339Nano), createdBy)
	if err != nil {
		return nil, fmt.Errorf("insert label: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit add label: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "add label"); err != nil {
		return nil, err
	}
	return s.ListLabels(ctx, in.IssueID)
}

func (s *Store) RemoveLabel(ctx context.Context, issueID, labelName string) ([]string, error) {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return nil, err
	}
	label, err := normalizeLabel(labelName)
	if err != nil {
		return nil, err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin remove label tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE issue_id = ? AND label = ?`, issueID, label)
	if err != nil {
		return nil, fmt.Errorf("delete label: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return nil, fmt.Errorf("label %q not found on issue %q", label, issueID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit remove label: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "remove label"); err != nil {
		return nil, err
	}
	return s.ListLabels(ctx, issueID)
}

func (s *Store) ReplaceLabels(ctx context.Context, issueID string, labels []string, createdBy string) error {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return err
	}
	normalized, err := canonicalizeLabels(labels)
	if err != nil {
		return err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin replace labels tx: %w", err)
	}
	defer tx.Rollback()
	if err := s.replaceLabelsTx(ctx, tx, issueID, normalized, createdBy); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace labels: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "replace labels"); err != nil {
		return err
	}
	return nil
}

func (s *Store) ListLabels(ctx context.Context, issueID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT label FROM labels WHERE issue_id = ? ORDER BY label ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	defer rows.Close()
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func (s *Store) replaceLabelsTx(ctx context.Context, tx *sql.Tx, issueID string, labels []string, createdBy string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE issue_id = ?`, issueID); err != nil {
		return fmt.Errorf("clear labels: %w", err)
	}
	author := strings.TrimSpace(createdBy)
	if author == "" {
		author = "unknown"
	}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx, `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES (?, ?, ?, ?)`, issueID, label, timestamp, author); err != nil {
			return fmt.Errorf("insert label %q: %w", label, err)
		}
	}
	return nil
}

func canonicalizeLabels(labels []string) ([]string, error) {
	out := make([]string, 0, len(labels))
	seen := map[string]struct{}{}
	for _, label := range labels {
		normalized, err := normalizeLabel(label)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeLabel(label string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(label))
	if normalized == "" {
		return "", errors.New("label is required")
	}
	if strings.Contains(normalized, ",") {
		return "", errors.New("label cannot contain commas")
	}
	return normalized, nil
}
