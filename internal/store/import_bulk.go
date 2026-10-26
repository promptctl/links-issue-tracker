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
	applied, _, err := s.bulkApply(ctx, prefix, actor, specs)
	return applied, err
}

// bulkApply is BulkApply, also returning the batch's revert point: the head
// the file's writes sit on, which withBatch owns. An undo names the commits it
// landed from it.
//
// [LAW:no-ambient-temporal-coupling] The read of the store as it stands, the
// merge and gate judged on it, and the batch share one commit-lock hold, so no
// write lands between what the merge read and what the batch writes. The
// observers hear what stands once the hold is released.
func (s *Store) bulkApply(ctx context.Context, prefix, actor string, specs []storage.BulkIssueSpec) (storage.BulkApplyResult, string, error) {
	var applied storage.BulkApplyResult
	var head string
	var heard []heardChange
	err := s.withCommitLock(ctx, func(ctx context.Context) error {
		ctx = context.WithValue(ctx, batchHearingKey{}, &heard)
		now, err := s.currentState(ctx, storage.ReadOutIDs(specs))
		if err != nil {
			return fmt.Errorf("bulk: read the issues the file was read out of: %w", err)
		}
		merged, err := storage.MergeReadOut(specs, now)
		if err != nil {
			return err
		}
		existingTypes, err := s.issueTypesByIDs(ctx, storage.BulkExistingRefs(merged))
		if err != nil {
			return fmt.Errorf("bulk: read the issues the file places: %w", err)
		}
		file, err := storage.ParseBulkFile(merged, existingTypes)
		if err != nil {
			return err
		}
		applied, head, err = s.writeBulkFile(ctx, prefix, actor, file, merged.Kept)
		return err
	})
	s.hear(ctx, heard...)
	return applied, head, err
}

// writeBulkFile writes a gated file as one batch, reporting the slots its
// merge kept beside what it wrote.
func (s *Store) writeBulkFile(ctx context.Context, prefix, actor string, file storage.BulkFile, kept []storage.Slot) (storage.BulkApplyResult, string, error) {
	return withBatch(ctx, s, func(ctx context.Context, result *storage.BulkApplyResult) error {
		result.Kept = kept
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
	})
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
	edges, err := queryRelations(w.ctx, w.tx, `relations WHERE src_id = ? AND type = ?`, id, string(relType))
	if err != nil {
		return nil, fmt.Errorf("%s relations of %s: %w", relType, id, err)
	}
	return edges, nil
}

func (w edgeWriterTx) Add(id, target string, relType model.RelationType) error {
	return addRelationTx(w.ctx, w.tx, model.Relation{SrcID: id, DstID: target, Type: relType, CreatedAt: w.now, CreatedBy: storage.Actor(w.actor)})
}

func (w edgeWriterTx) Remove(edge model.Relation) error {
	_, err := deleteRelationRowTx(w.ctx, w.tx, relationKey{srcID: edge.SrcID, dstID: edge.DstID, kind: edge.Type})
	return err
}
