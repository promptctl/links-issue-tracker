package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
)

type AddRelationInput struct {
	SrcID     string
	DstID     string
	Type      string
	CreatedBy string
}

type SetParentInput struct {
	ChildID   string
	ParentID  string
	CreatedBy string
}

func (s *Store) AddRelation(ctx context.Context, in AddRelationInput) (model.Relation, error) {
	if _, err := s.GetIssue(ctx, in.SrcID); err != nil {
		return model.Relation{}, err
	}
	if _, err := s.GetIssue(ctx, in.DstID); err != nil {
		return model.Relation{}, err
	}
	relType := strings.TrimSpace(in.Type)
	if relType != "blocks" && relType != "parent-child" && relType != "related-to" {
		return model.Relation{}, errors.New("relation type must be blocks, parent-child, or related-to")
	}
	srcID, dstID := in.SrcID, in.DstID
	if relType == "related-to" {
		if srcID == dstID {
			return model.Relation{}, errors.New("related-to cannot target itself")
		}
		ordered := []string{srcID, dstID}
		sort.Strings(ordered)
		srcID, dstID = ordered[0], ordered[1]
	}
	now := time.Now().UTC()
	rel := model.Relation{SrcID: srcID, DstID: dstID, Type: relType, CreatedAt: now, CreatedBy: strings.TrimSpace(in.CreatedBy)}
	if rel.CreatedBy == "" {
		rel.CreatedBy = "unknown"
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return model.Relation{}, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Relation{}, fmt.Errorf("begin add relation tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)`, rel.SrcID, rel.DstID, rel.Type, rel.CreatedAt.Format(time.RFC3339Nano), rel.CreatedBy); err != nil {
		return model.Relation{}, fmt.Errorf("insert relation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Relation{}, fmt.Errorf("commit add relation: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "add relation"); err != nil {
		return model.Relation{}, err
	}
	return rel, nil
}

func (s *Store) RemoveRelation(ctx context.Context, srcID, dstID, relType string) error {
	if relType == "related-to" {
		ordered := []string{srcID, dstID}
		sort.Strings(ordered)
		srcID, dstID = ordered[0], ordered[1]
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin remove relation tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND dst_id = ? AND type = ?`, srcID, dstID, relType)
	if err != nil {
		return fmt.Errorf("delete relation: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("relation not found")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit remove relation: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "remove relation"); err != nil {
		return err
	}
	return nil
}

func (s *Store) ListRelationsForIssue(ctx context.Context, issueID string, relType string) ([]model.Relation, error) {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return nil, err
	}
	rels, err := s.listRelations(ctx, issueID)
	if err != nil {
		return nil, err
	}
	normalizedType := strings.TrimSpace(relType)
	if normalizedType == "" {
		return rels, nil
	}
	out := make([]model.Relation, 0, len(rels))
	for _, rel := range rels {
		if rel.Type == normalizedType {
			out = append(out, rel)
		}
	}
	return out, nil
}

func (s *Store) SetParent(ctx context.Context, in SetParentInput) (model.Relation, error) {
	if strings.TrimSpace(in.ChildID) == "" || strings.TrimSpace(in.ParentID) == "" {
		return model.Relation{}, errors.New("child and parent ids are required")
	}
	if in.ChildID == in.ParentID {
		return model.Relation{}, errors.New("child and parent cannot be the same issue")
	}
	if _, err := s.GetIssue(ctx, in.ChildID); err != nil {
		return model.Relation{}, err
	}
	if _, err := s.GetIssue(ctx, in.ParentID); err != nil {
		return model.Relation{}, err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return model.Relation{}, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Relation{}, fmt.Errorf("begin set parent tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND type = 'parent-child'`, in.ChildID); err != nil {
		return model.Relation{}, fmt.Errorf("clear parent relation: %w", err)
	}
	rel := model.Relation{
		SrcID:     in.ChildID,
		DstID:     in.ParentID,
		Type:      "parent-child",
		CreatedAt: time.Now().UTC(),
		CreatedBy: strings.TrimSpace(in.CreatedBy),
	}
	if rel.CreatedBy == "" {
		rel.CreatedBy = "unknown"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, ?)`, rel.SrcID, rel.DstID, rel.CreatedAt.Format(time.RFC3339Nano), rel.CreatedBy); err != nil {
		return model.Relation{}, fmt.Errorf("insert parent relation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Relation{}, fmt.Errorf("commit set parent: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "set parent"); err != nil {
		return model.Relation{}, err
	}
	return rel, nil
}

func (s *Store) ClearParent(ctx context.Context, childID string) error {
	if _, err := s.GetIssue(ctx, childID); err != nil {
		return err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin clear parent tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND type = 'parent-child'`, childID)
	if err != nil {
		return fmt.Errorf("delete parent relation: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return NotFoundError{Entity: "parent relation", ID: childID}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit clear parent: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "clear parent"); err != nil {
		return err
	}
	return nil
}

func (s *Store) ListChildren(ctx context.Context, parentID string) ([]model.Issue, error) {
	if _, err := s.GetIssue(ctx, parentID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.title, i.description, i.agent_prompt, i.status, i.priority, i.issue_type, i.topic, i.assignee, i.item_rank, i.created_at, i.updated_at, i.closed_at, i.archived_at, i.deleted_at
		FROM relations r
		JOIN issues i ON i.id = r.src_id
		WHERE r.type = 'parent-child' AND r.dst_id = ?
		ORDER BY i.item_rank ASC, i.id ASC`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list children: %w", err)
	}
	defer rows.Close()
	children := []issueRow{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		children = append(children, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.hydrateIssues(ctx, children)
}
