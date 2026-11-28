package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// ApplyConfig puts content in force by storage.NextConfigApply's rule, read
// against the applies inside the same transaction that writes, so two applies
// racing on one store each see the other's.
func (s *Store) ApplyConfig(ctx context.Context, content string, actor string) (model.ConfigApply, bool, error) {
	var apply model.ConfigApply
	var wrote bool
	err := s.withMutation(ctx, "apply config", func(ctx context.Context, tx *sql.Tx) error {
		applies, err := queryConfigApplies(ctx, tx)
		if err != nil {
			return err
		}
		apply, wrote = storage.NextConfigApply(applies, model.ID("cfg-"+uuid.NewString()), content, actor, &s.stamps, s.clock)
		if !wrote {
			return nil
		}
		return insertConfigApplyTx(ctx, tx, apply)
	})
	if err != nil {
		return model.ConfigApply{}, false, err
	}
	return apply, wrote, nil
}

// ConfigApplies is every apply, in model.ConfigApplyOrdering.
func (s *Store) ConfigApplies(ctx context.Context) ([]model.ConfigApply, error) {
	return queryConfigApplies(ctx, s.db)
}

// insertConfigApplyTx is the one INSERT into config_applies, shared by an
// apply and a restore or replay. [LAW:single-enforcer]
func insertConfigApplyTx(ctx context.Context, tx *sql.Tx, apply model.ConfigApply) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO config_applies(id, content, created_at, created_by) VALUES (?, ?, ?, ?)`,
		apply.ID, apply.Content, model.StampText(apply.CreatedAt), apply.CreatedBy); err != nil {
		return fmt.Errorf("insert config apply %s: %w", apply.ID, err)
	}
	return nil
}

func deleteConfigApplyTx(ctx context.Context, tx *sql.Tx, id model.ID) (int64, error) {
	return execDelete(ctx, tx, `DELETE FROM config_applies WHERE id = ?`, fmt.Sprintf("config apply %s", id), id)
}

// queryConfigApplies reads every apply, in model.ConfigApplyOrdering. The
// order is taken in Go, not in SQL, so the model's rule is the one both
// engines sort by.
//
// A store below migration v13 has no config_applies table, and nothing has
// been applied to it: a backlog a schema hold keeps below v13, or a sync
// session's store before it migrates. It holds no applies and runs on the
// defaults, as queryAsOf reads a table a commit predates. An apply on such a
// store still fails, at its INSERT. [LAW:no-defensive-null-guards] the absent
// table is a real domain value, matched here at the backend boundary.
func queryConfigApplies(ctx context.Context, q rowQueryer) ([]model.ConfigApply, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, content, created_at, created_by FROM config_applies`)
	if isMissingTableError(err) {
		return []model.ConfigApply{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list config applies: %w", err)
	}
	defer rows.Close()
	out := []model.ConfigApply{}
	for rows.Next() {
		var apply model.ConfigApply
		var createdAt string
		if err := rows.Scan(&apply.ID, &apply.Content, &createdAt, &apply.CreatedBy); err != nil {
			return nil, err
		}
		if apply.CreatedAt, err = scanTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, apply)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, model.ConfigApplyOrdering)
	return out, nil
}
