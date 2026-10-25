package store

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// ImportTree validates and creates a tree of issues described by specs in
// dependency order, as one batch (withBatch): parents and BlockedBy referents
// are created first, then their children/dependents. Parent relations are
// wired by CreateIssue itself (via ParentID); blocked-by edges are added in a
// second pass. A tree refused at any write writes nothing.
//
// [LAW:single-enforcer] Atomic tree import is the one shared boundary that
// owns ID resolution; what it creates, and in what order, is the file
// storage.ParseImportTreeFile parsed once it judged it.
func (s *Store) ImportTree(ctx context.Context, prefix, actor string, specs []storage.ImportTreeSpec) (storage.ImportTreeResult, error) {
	creates, err := storage.ParseImportTreeFile(specs)
	if err != nil {
		return storage.ImportTreeResult{}, err
	}
	return withBatch(ctx, s, func(ctx context.Context, result *storage.ImportTreeResult) error {
		idMap := make(map[string]string, len(creates))
		createdIDs := make([]string, 0, len(creates))
		for _, create := range creates {
			issue, err := s.CreateIssue(ctx, create.Input(idMap[create.Parent], prefix, actor))
			if err != nil {
				return fmt.Errorf("import: create %q: %w", create.LocalID, err)
			}
			idMap[create.LocalID] = issue.ID
			result.Created = append(result.Created, storage.NewIDMapping(create.LocalID, issue.ID))
			createdIDs = append(createdIDs, issue.ID)
		}
		for i, create := range creates {
			for _, dep := range create.BlockedBy {
				if _, err := s.AddRelation(ctx, storage.AddRelationInput{SrcID: createdIDs[i], DstID: idMap[dep], Type: model.RelBlockedBy, CreatedBy: actor}); err != nil {
					return fmt.Errorf("import: blocked_by %q -> %q: %w", create.LocalID, dep, err)
				}
			}
		}
		return nil
	})
}
