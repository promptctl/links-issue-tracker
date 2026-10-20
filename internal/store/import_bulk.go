package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// BulkApply creates or updates every issue named by specs: it creates the
// file's new issues in the order storage.ParseBulkFile hands back, writes the
// update documents' reparents as one transaction, wires the creates'
// blocked_by edges, writes the update documents' blocked_by sets as one
// transaction, writes the file's ordering as one RankSet, then applies each
// update document's patch. Reparents come before the edges so each edge is
// judged on the hierarchy the file ends with, and before the ordering because
// `rank: top` stacks an issue in the frame its parent makes. On any failure
// mid-batch, BulkApply puts back the edges its blocked_by sets replaced and
// the parents its reparents replaced, best-effort rolls back issues it created in this call (never issues it
// updated — an update has no "prior create" to unwind) and returns what stays
// written beside the error; updates come last,
// so a failing create, reparent, edge, or ordering leaves no update applied.
// Run `lit doctor` after a failed batch to detect orphans left if rollback
// itself failed. [LAW:single-enforcer] one boundary owns ID
// resolution; what it writes is the file storage.ParseBulkFile parsed once
// it judged it.
func (s *Store) BulkApply(ctx context.Context, prefix, actor string, specs []storage.BulkIssueSpec) (storage.BulkApplyResult, error) {
	existingTypes, err := s.issueTypesByIDs(ctx, storage.BulkExistingRefs(specs))
	if err != nil {
		return storage.BulkApplyResult{}, fmt.Errorf("bulk: read the issues the file places: %w", err)
	}
	file, err := storage.ParseBulkFile(specs, existingTypes)
	if err != nil {
		return storage.BulkApplyResult{}, err
	}

	result := storage.BulkApplyResult{}
	createdIDs := make([]string, 0, len(file.Creates))
	var reparents storage.ReparentStep
	var blockedBy storage.BlockedByStep
	refuse := func(err error) (storage.BulkApplyResult, error) {
		if len(blockedBy.Set) > 0 {
			if undoErr := s.withMutation(ctx, "bulk blocked_by undo", func(ctx context.Context, tx *sql.Tx) error {
				return blockedBy.Undo(blockedByWriterTx{ctx: ctx, tx: tx, actor: actor, now: s.clock.Now()})
			}); undoErr != nil {
				err = fmt.Errorf("%w; %w", err, undoErr)
			} else {
				result.BlockedByUndone, result.BlockedBySet = result.BlockedBySet, nil
			}
		}
		if len(reparents.Moved) > 0 {
			if undoErr := s.withMutation(ctx, "bulk reparent undo", func(ctx context.Context, tx *sql.Tx) error {
				return reparents.Undo(parentWriterTx{ctx: ctx, tx: tx, actor: actor, now: s.clock.Now()})
			}); undoErr != nil {
				err = fmt.Errorf("%w; %w", err, undoErr)
			} else {
				result.ReparentsUndone, result.Reparented = result.Reparented, nil
			}
		}
		leaked := s.rollbackCreatedIssues(ctx, actor, createdIDs)
		return result.AfterRollback(leaked), fmt.Errorf("%w; %s (rollback leaked %d: %s)", err, result.Kept(leaked), len(leaked), strings.Join(leaked, ","))
	}
	localRealID := make(map[string]string, len(file.Creates))
	resolve := func(ref string) string { return resolveBulkRef(ref, localRealID) }
	for _, create := range file.Creates {
		issue, err := s.CreateIssue(ctx, create.Input(resolveBulkRef(create.Parent, localRealID), prefix, actor))
		if err != nil {
			return refuse(fmt.Errorf("bulk: create doc %d: %w", create.Index, err))
		}
		createdIDs = append(createdIDs, issue.ID)
		if create.LocalID != "" {
			localRealID[create.LocalID] = issue.ID
		}
		result.Created = append(result.Created, storage.NewIDMapping(create.LocalID, issue.ID))
	}
	if len(file.Reparents) > 0 {
		if err := s.withMutation(ctx, "bulk reparent", func(ctx context.Context, tx *sql.Tx) error {
			var err error
			reparents, err = file.WriteReparents(resolve, parentWriterTx{ctx: ctx, tx: tx, actor: actor, now: s.clock.Now()})
			return err
		}); err != nil {
			reparents = storage.ReparentStep{}
			return refuse(err)
		}
		result.Reparented, result.ParentHeld = reparents.Moved, reparents.Held
	}
	for i, create := range file.Creates {
		for _, dep := range create.BlockedBy {
			if _, err := s.AddRelation(ctx, storage.AddRelationInput{SrcID: createdIDs[i], DstID: resolveBulkRef(dep, localRealID), Type: model.RelBlockedBy, CreatedBy: actor}); err != nil {
				return refuse(fmt.Errorf("bulk: blocked_by doc %d -> %q: %w", create.Index, dep, err))
			}
		}
	}
	if len(file.BlockedBy) > 0 {
		if err := s.withMutation(ctx, "bulk blocked_by", func(ctx context.Context, tx *sql.Tx) error {
			var err error
			blockedBy, err = file.WriteBlockedBy(resolve, blockedByWriterTx{ctx: ctx, tx: tx, actor: actor, now: s.clock.Now()})
			return err
		}); err != nil {
			blockedBy = storage.BlockedByStep{}
			return refuse(err)
		}
		result.BlockedBySet, result.BlockedByHeld = blockedBy.Set, blockedBy.Held
		result.BlockedByAdded, result.BlockedByRemoved = blockedBy.Added, blockedBy.Removed
	}
	if ranked := file.RankedIDs(createdIDs); len(ranked) > 0 {
		if result.Ranked, err = s.RankSet(ctx, ranked); err != nil {
			return refuse(fmt.Errorf("bulk: rank: %w", err))
		}
	}
	for _, update := range file.Updates {
		applied, err := s.Apply(ctx, update.ID, storage.Change{Actor: actor, Fields: update.Patch})
		if err != nil {
			return refuse(fmt.Errorf("bulk: update %q: %w", update.ID, err))
		}
		result.Updated = append(result.Updated, applied.Issue.ID)
	}
	return result, nil
}

// parentWriterTx is a bulk file's hierarchy write on one tx: the writes `lit
// parent set` and `lit parent clear` make, so a bulk reparent obeys the one
// hierarchy rule. [LAW:single-enforcer]
type parentWriterTx struct {
	ctx   context.Context
	tx    *sql.Tx
	actor string
	now   time.Time
}

func (w parentWriterTx) edge(childID, parentID string) model.Relation {
	return model.Relation{SrcID: childID, DstID: parentID, Type: model.RelParentChild, CreatedAt: w.now, CreatedBy: storage.Actor(w.actor)}
}

// Holds asks the question every relation write asks first: whether the state
// the edge would make already holds.
func (w parentWriterTx) Holds(childID, parentID string) (bool, error) {
	err := rejectHeldRelationTx(w.ctx, w.tx, w.edge(childID, parentID))
	if held := (storage.RelationExistsError{}); errors.As(err, &held) {
		return true, nil
	}
	return false, err
}

func (w parentWriterTx) Clear(childID string) error {
	return clearParentTx(w.ctx, w.tx, childID)
}

func (w parentWriterTx) Detach(childID string) error {
	_, err := detachParentsTx(w.ctx, w.tx, childID)
	return err
}

func (w parentWriterTx) Attach(childID, parentID string) error {
	return addRelationTx(w.ctx, w.tx, w.edge(childID, parentID))
}

func (w parentWriterTx) Parents(childID string) ([]model.Relation, error) {
	return edgesFromTx(w.ctx, w.tx, childID, model.RelParentChild)
}

// edgesFromTx lists the edges of relType srcID holds on tx, as stored.
func edgesFromTx(ctx context.Context, tx *sql.Tx, srcID string, relType model.RelationType) ([]model.Relation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT src_id, dst_id, type, created_at, created_by FROM relations WHERE src_id = ? AND type = ?`, srcID, string(relType))
	if err != nil {
		return nil, fmt.Errorf("list %s relations of %s: %w", relType, srcID, err)
	}
	defer rows.Close()
	var edges []model.Relation
	for rows.Next() {
		var rel model.Relation
		var createdAt string
		if err := rows.Scan(&rel.SrcID, &rel.DstID, &rel.Type, &createdAt, &rel.CreatedBy); err != nil {
			return nil, err
		}
		if rel.CreatedAt, err = scanTime(createdAt); err != nil {
			return nil, err
		}
		edges = append(edges, rel)
	}
	return edges, rows.Err()
}

// Restore writes edges back raw, as import restore does: they are the rows
// Parents read, so no rule judges them again. [LAW:one-source-of-truth]
func (w parentWriterTx) Restore(childID string, edges []model.Relation) error {
	if _, err := detachParentsTx(w.ctx, w.tx, childID); err != nil {
		return err
	}
	for _, rel := range edges {
		if err := insertRelationTx(w.ctx, w.tx, rel); err != nil {
			return err
		}
	}
	return nil
}

// blockedByWriterTx is a bulk file's dependency write on one tx: the writes
// `lit dep add` and `lit dep rm` make, so a bulk blocked_by set obeys the one
// rule every blocked-by edge obeys. [LAW:single-enforcer]
type blockedByWriterTx struct {
	ctx   context.Context
	tx    *sql.Tx
	actor string
	now   time.Time
}

func (w blockedByWriterTx) Blockers(id string) ([]model.Relation, error) {
	return edgesFromTx(w.ctx, w.tx, id, model.RelBlockedBy)
}

func (w blockedByWriterTx) Add(id, blockerID string) error {
	return addRelationTx(w.ctx, w.tx, model.Relation{SrcID: id, DstID: blockerID, Type: model.RelBlockedBy, CreatedAt: w.now, CreatedBy: storage.Actor(w.actor)})
}

func (w blockedByWriterTx) Remove(id, blockerID string) error {
	_, err := deleteRelationRowTx(w.ctx, w.tx, relationKey{srcID: id, dstID: blockerID, kind: model.RelBlockedBy})
	return err
}

// Restore writes edges back raw, as parentWriterTx.Restore does: they are the
// rows Blockers read, so no rule judges them again.
func (w blockedByWriterTx) Restore(id string, edges []model.Relation) error {
	current, err := w.Blockers(id)
	if err != nil {
		return err
	}
	for _, rel := range current {
		if err := w.Remove(id, rel.DstID); err != nil {
			return err
		}
	}
	for _, rel := range edges {
		if err := insertRelationTx(w.ctx, w.tx, rel); err != nil {
			return err
		}
	}
	return nil
}

// resolveBulkRef resolves a parent/blocked_by reference against this batch's
// LocalID -> real-ID map; a reference that matches nothing in the batch
// passes through unchanged as a real, pre-existing issue ID for CreateIssue
// or AddRelation to validate. [LAW:dataflow-not-control-flow] one
// unconditional lookup-with-fallback, not a branch on "is this local or
// external."
func resolveBulkRef(ref string, localRealID map[string]string) string {
	if real, ok := localRealID[ref]; ok {
		return real
	}
	return ref
}
