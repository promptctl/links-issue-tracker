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
		var id, createdAt string
		if err := tx.QueryRowContext(ctx, `SELECT id, created_at FROM issues WHERE id = ?`, in.IssueID).Scan(&id, &createdAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storage.NotFoundError{Entity: "issue", ID: in.IssueID}
			}
			return fmt.Errorf("read issue %s: %w", in.IssueID, err)
		}
		born, err := scanTime(createdAt)
		if err != nil {
			return err
		}
		children, err := detachedChildrenTx(ctx, tx, id)
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
			(SELECT COUNT(*) FROM issue_events WHERE issue_id = ?),
			(SELECT COUNT(*) FROM plan_reviews WHERE issue_id = ?)`,
			id, id, id, id, id, id).Scan(&result.Comments, &result.Labels, &result.Relations, &result.Events, &result.PlanReviews); err != nil {
			return fmt.Errorf("count rows of issue %s: %w", id, err)
		}
		// [LAW:single-enforcer] one issues DELETE, shared with the replay's
		// delta; ON DELETE CASCADE takes the comments, labels, history, plan
		// reviews and edges.
		if _, err := deleteIssueTx(ctx, tx, id); err != nil {
			return err
		}
		result.Purge = model.Purge{IssueID: id, IssueCreatedAt: born, PurgedAt: s.clock.Now(), PurgedBy: in.Actor}
		return insertPurgeTx(ctx, tx, result.Purge)
	})
	if err != nil {
		return storage.PurgeResult{}, err
	}
	return result, nil
}

// scanPurge decodes one purges row selected as issue_id, issue_created_at,
// purged_at, purged_by — the order listPurges and purgesAtCommit both select.
func scanPurge(rows *sql.Rows) (model.Purge, error) {
	var p model.Purge
	var createdAt, purgedAt string
	if err := rows.Scan(&p.IssueID, &createdAt, &purgedAt, &p.PurgedBy); err != nil {
		return model.Purge{}, err
	}
	var err error
	if p.IssueCreatedAt, err = scanTime(createdAt); err != nil {
		return model.Purge{}, err
	}
	if p.PurgedAt, err = scanTime(purgedAt); err != nil {
		return model.Purge{}, err
	}
	return p, nil
}

// detachedChildrenTx reads every child filed under parentID, with the parent
// edges it keeps once its edge to parentID is gone.
func detachedChildrenTx(ctx context.Context, tx *sql.Tx, parentID string) ([]storage.DetachedChild, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT r.src_id, c.issue_type, COUNT(o.dst_id)
FROM relations r
JOIN issues c ON c.id = r.src_id
LEFT JOIN relations o ON o.src_id = r.src_id AND o.type = 'parent-child' AND o.dst_id <> r.dst_id
WHERE r.dst_id = ? AND r.type = 'parent-child'
GROUP BY r.src_id, c.issue_type`, parentID)
	if err != nil {
		return nil, fmt.Errorf("read children of %s: %w", parentID, err)
	}
	defer rows.Close()
	var out []storage.DetachedChild
	for rows.Next() {
		var child storage.DetachedChild
		if err := rows.Scan(&child.ID, &child.Type, &child.ParentsLeft); err != nil {
			return nil, err
		}
		out = append(out, child)
	}
	return out, rows.Err()
}

// insertPurgeTx writes one purge record. It is the only INSERT into purges:
// a purge writes its record through it, and so do a restore and the replay's
// delta, which carry records another store wrote.
func insertPurgeTx(ctx context.Context, tx *sql.Tx, p model.Purge) error {
	key := p.Key()
	if _, err := tx.ExecContext(ctx, `INSERT INTO purges(issue_id, issue_created_at, purged_at, purged_by) VALUES (?, ?, ?, ?)`,
		key.IssueID, key.IssueCreatedAt, p.PurgedAt.Format(time.RFC3339Nano), p.PurgedBy); err != nil {
		return fmt.Errorf("record purge of %s: %w", p.IssueID, err)
	}
	return nil
}

// listPurges reads every purge record, by issue id and then birth ascending,
// the order model.UnionPurges gives an export. It takes a
// rowQueryer because Export reads it outside any transaction and a restore
// reads it inside the one it is about to rewrite the tables in.
func listPurges(ctx context.Context, q rowQueryer) ([]model.Purge, error) {
	rows, err := q.QueryContext(ctx, `SELECT issue_id, issue_created_at, purged_at, purged_by FROM purges`)
	if err != nil {
		return nil, fmt.Errorf("list purges: %w", err)
	}
	defer rows.Close()
	out := []model.Purge{}
	for rows.Next() {
		p, err := scanPurge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// [LAW:one-source-of-truth] the order is UnionPurges', not a second
	// statement of it in SQL; each ticket holds one record, so the union only
	// sorts.
	return model.UnionPurges(out), nil
}
