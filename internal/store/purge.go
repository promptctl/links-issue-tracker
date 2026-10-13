package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Purge permanently removes an issue and leaves its purge record, in one
// mutation and so one Dolt commit. Existence, the containment refusal, the
// counts, the delete and the record are all read and written inside that one
// transaction, so the row proven present is the row removed and the counts
// describe exactly what the cascade took. [LAW:single-enforcer]
func (s *Store) Purge(ctx context.Context, in storage.PurgeInput) (storage.PurgeResult, error) {
	var result storage.PurgeResult
	err := s.withMutation(ctx, "purge issue", func(ctx context.Context, tx *sql.Tx) error {
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE id = ?`, in.IssueID).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storage.NotFoundError{Entity: "issue", ID: in.IssueID}
			}
			return fmt.Errorf("read issue %s: %w", in.IssueID, err)
		}
		children, err := countChildren(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := storage.RejectPurgeOfOccupiedContainer(id, children); err != nil {
			return err
		}
		// The cascade removes these rows without reporting them, so they are
		// counted first; the delete below is in the same transaction, so nothing
		// can land between the count and the removal it describes.
		if err := tx.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM comments WHERE issue_id = ?),
			(SELECT COUNT(*) FROM labels WHERE issue_id = ?),
			(SELECT COUNT(*) FROM relations WHERE src_id = ? OR dst_id = ?),
			(SELECT COUNT(*) FROM issue_events WHERE issue_id = ?)`,
			id, id, id, id, id).Scan(&result.Comments, &result.Labels, &result.Relations, &result.Events); err != nil {
			return fmt.Errorf("count rows of issue %s: %w", id, err)
		}
		// [LAW:single-enforcer] one issues DELETE, shared with the replay's
		// delta; ON DELETE CASCADE takes the comments, labels, history and edges.
		if _, err := deleteIssueTx(ctx, tx, id); err != nil {
			return err
		}
		result.Purge = model.Purge{IssueID: id, PurgedAt: s.clock.Now(), PurgedBy: in.Actor}
		return insertPurgeTx(ctx, tx, result.Purge)
	})
	if err != nil {
		return storage.PurgeResult{}, err
	}
	return result, nil
}

// insertPurgeTx writes one purge record. It is the only INSERT into purges:
// a purge writes its record through it, and so do a restore and the replay's
// delta, which carry records another store wrote.
func insertPurgeTx(ctx context.Context, tx *sql.Tx, p model.Purge) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO purges(issue_id, purged_at, purged_by) VALUES (?, ?, ?)`,
		p.IssueID, p.PurgedAt.Format(time.RFC3339Nano), p.PurgedBy); err != nil {
		return fmt.Errorf("record purge of %s: %w", p.IssueID, err)
	}
	return nil
}

// listPurges reads every purge record, by issue id ascending. It takes a
// rowQueryer because Export reads it outside any transaction and a restore
// reads it inside the one it is about to rewrite the tables in.
func listPurges(ctx context.Context, q rowQueryer) ([]model.Purge, error) {
	rows, err := q.QueryContext(ctx, `SELECT issue_id, purged_at, purged_by FROM purges ORDER BY issue_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list purges: %w", err)
	}
	defer rows.Close()
	out := []model.Purge{}
	for rows.Next() {
		var p model.Purge
		var purgedAt string
		if err := rows.Scan(&p.IssueID, &purgedAt, &p.PurgedBy); err != nil {
			return nil, err
		}
		if p.PurgedAt, err = scanTime(purgedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
