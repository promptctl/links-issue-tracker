package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// BulkApply creates or updates every issue named by specs: it creates the
// file's new issues in the order storage.ParseBulkFile hands back, wires
// their blocked_by edges, then applies each update document's patch. On any
// failure mid-batch, BulkApply best-effort rolls back issues it created in
// this call (never issues it updated — an update has no "prior create" to
// unwind); updates come after creates and edges, so a failing create or edge
// leaves no update applied, and the file's ordering comes last, as one
// RankSet, so it is applied whole or not at all. Run `lit doctor` after a failed batch to detect orphans left if
// rollback itself failed. [LAW:single-enforcer] one boundary owns ID
// resolution; what it writes is the file storage.ParseBulkFile parsed once
// it judged it.
func (s *Store) BulkApply(ctx context.Context, prefix, actor string, specs []storage.BulkIssueSpec) (storage.BulkApplyResult, error) {
	parents, err := s.getIssuesByIDs(ctx, storage.BulkParentRefs(specs))
	if err != nil {
		return storage.BulkApplyResult{}, fmt.Errorf("bulk: read parents: %w", err)
	}
	parentTypes := make(map[string]model.IssueType, len(parents))
	for id, parent := range parents {
		parentTypes[id] = parent.IssueType
	}
	file, err := storage.ParseBulkFile(specs, parentTypes)
	if err != nil {
		return storage.BulkApplyResult{}, err
	}

	result := storage.BulkApplyResult{}
	createdIDs := make([]string, 0, len(file.Creates))
	createdByIndex := make(map[int]string, len(file.Creates))
	localRealID := make(map[string]string, len(file.Creates))
	for _, create := range file.Creates {
		issue, err := s.CreateIssue(ctx, create.Input(resolveBulkRef(create.Parent, localRealID), prefix, actor))
		if err != nil {
			leaked := s.rollbackCreatedIssues(ctx, actor, createdIDs)
			return storage.BulkApplyResult{}, fmt.Errorf("bulk: create doc %d: %w (rollback leaked %d: %s)", create.Index, err, len(leaked), strings.Join(leaked, ","))
		}
		createdIDs = append(createdIDs, issue.ID)
		createdByIndex[create.Index] = issue.ID
		if create.LocalID != "" {
			localRealID[create.LocalID] = issue.ID
		}
		result.Created = append(result.Created, storage.NewIDMapping(create.LocalID, issue.ID))
	}
	for i, create := range file.Creates {
		for _, dep := range create.BlockedBy {
			if _, err := s.AddRelation(ctx, storage.AddRelationInput{SrcID: createdIDs[i], DstID: resolveBulkRef(dep, localRealID), Type: model.RelBlockedBy, CreatedBy: actor}); err != nil {
				leaked := s.rollbackCreatedIssues(ctx, actor, createdIDs)
				return storage.BulkApplyResult{}, fmt.Errorf("bulk: blocked_by doc %d -> %q: %w (rollback leaked %d: %s)", create.Index, dep, err, len(leaked), strings.Join(leaked, ","))
			}
		}
	}
	for _, update := range file.Updates {
		applied, err := s.Apply(ctx, update.ID, storage.Change{Actor: actor, Fields: update.Patch})
		if err != nil {
			leaked := s.rollbackCreatedIssues(ctx, actor, createdIDs)
			return storage.BulkApplyResult{}, fmt.Errorf("bulk: update %q: %w (rollback leaked %d: %s)", update.ID, err, len(leaked), strings.Join(leaked, ","))
		}
		result.Updated = append(result.Updated, applied.Issue.ID)
	}
	if ranked := file.RankedIDs(func(index int) string { return createdByIndex[index] }); len(ranked) > 0 {
		if result.Ranked, err = s.RankSet(ctx, ranked); err != nil {
			leaked := s.rollbackCreatedIssues(ctx, actor, createdIDs)
			return storage.BulkApplyResult{}, fmt.Errorf("bulk: rank: %w (rollback leaked %d: %s)", err, len(leaked), strings.Join(leaked, ","))
		}
	}
	return result, nil
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
