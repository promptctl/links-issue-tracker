package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// BulkApply creates or updates every issue named by specs, as one batch
// (withBatch): it creates the file's new issues in the order
// storage.ParseBulkFile hands back, writes every edge set the file states —
// the update documents' parents and blocked_by, and the creates' blocked_by —
// as one transaction judged on the graph the file ends with, writes the file's
// ordering as one RankSet, then applies each update document's patch. The
// edges come before the ordering because `rank: top` stacks an issue in the
// frame its parent makes. A file refused at any write writes nothing, and
// withBatch holds the observers' hearing of its updates until they stand.
// [LAW:single-enforcer] one boundary owns ID resolution; what it writes is the
// file storage.ParseBulkFile parsed once it judged it.
func (s *Store) BulkApply(ctx context.Context, prefix, actor string, specs []storage.BulkIssueSpec) (storage.BulkApplyResult, error) {
	existingTypes, err := s.issueTypesByIDs(ctx, storage.BulkExistingRefs(specs))
	if err != nil {
		return storage.BulkApplyResult{}, fmt.Errorf("bulk: read the issues the file places: %w", err)
	}
	file, err := storage.ParseBulkFile(specs, existingTypes)
	if err != nil {
		return storage.BulkApplyResult{}, err
	}

	var result storage.BulkApplyResult
	if err := s.withBatch(ctx, func(ctx context.Context) error {
		createdIDs := make([]string, 0, len(file.Creates))
		localRealID := make(map[string]string, len(file.Creates))
		for _, create := range file.Creates {
			issue, err := s.CreateIssue(ctx, create.Input(storage.ResolveRef(create.Parent, localRealID), prefix, actor))
			if err != nil {
				return fmt.Errorf("bulk: create doc %d: %w", create.Index, err)
			}
			createdIDs = append(createdIDs, issue.ID)
			if create.LocalID != "" {
				localRealID[create.LocalID] = issue.ID
			}
			result.Created = append(result.Created, storage.NewIDMapping(create.LocalID, issue.ID))
		}
		var edges storage.EdgeStep
		if err := s.withMutation(ctx, "bulk edges", func(ctx context.Context, tx *sql.Tx) error {
			var err error
			edges, err = file.WriteEdges(createdIDs, edgeWriterTx{ctx: ctx, tx: tx, actor: actor, now: s.clock.Now()})
			return err
		}); err != nil {
			return err
		}
		result.RecordEdges(edges)
		if ranked := file.RankedIDs(createdIDs); len(ranked) > 0 {
			var err error
			if result.Ranked, err = s.RankSet(ctx, ranked); err != nil {
				return fmt.Errorf("bulk: rank: %w", err)
			}
		}
		for _, update := range file.Updates {
			applied, err := s.Apply(ctx, update.ID, storage.Change{Actor: actor, Fields: update.Patch})
			if err != nil {
				return fmt.Errorf("bulk: update %q: %w", update.ID, err)
			}
			result.RecordUpdate(applied)
		}
		return nil
	}); err != nil {
		return refusedBatch(result, err)
	}
	return result, nil
}

// edgeWriterTx is a bulk file's relation write on one tx: every edge it adds
// goes through addRelationTx, the write `lit dep add` and `lit parent set`
// make, so an edge a file writes obeys the one rule its type carries.
// [LAW:single-enforcer]
type edgeWriterTx struct {
	ctx   context.Context
	tx    *sql.Tx
	actor string
	now   time.Time
}

func (w edgeWriterTx) Edges(id string, relType model.RelationType) ([]model.Relation, error) {
	if err := requireIssueExistsTx(w.ctx, w.tx, id); err != nil {
		return nil, err
	}
	rows, err := w.tx.QueryContext(w.ctx, `SELECT src_id, dst_id, type, created_at, created_by FROM relations WHERE src_id = ? AND type = ?`, id, string(relType))
	if err != nil {
		return nil, fmt.Errorf("list %s relations of %s: %w", relType, id, err)
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

func (w edgeWriterTx) Add(id, target string, relType model.RelationType) error {
	return addRelationTx(w.ctx, w.tx, model.Relation{SrcID: id, DstID: target, Type: relType, CreatedAt: w.now, CreatedBy: storage.Actor(w.actor)})
}

func (w edgeWriterTx) Remove(edge model.Relation) error {
	_, err := deleteRelationRowTx(w.ctx, w.tx, relationKey{srcID: edge.SrcID, dstID: edge.DstID, kind: edge.Type})
	return err
}
