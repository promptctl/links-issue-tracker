package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// ImportTree validates and creates a tree of issues described by specs in
// dependency order: parents and BlockedBy referents are created first, then
// their children/dependents. Parent relations are wired by CreateIssue itself
// (via ParentID); blocked-by edges are added in a second pass.
//
// On any failure mid-import, ImportTree best-effort rolls back already-created
// issues by transitioning them to deleted. Atomicity is therefore best-effort:
// if rollback itself fails, partial state remains and the surfaced error
// names every step that's left dangling. Run `lit doctor` after a failed
// import to detect orphans.
//
// [LAW:single-enforcer] Atomic tree import is the one shared boundary that
// owns ID resolution; what it creates, and in what order, is the file
// storage.ParseImportTreeFile parsed once it judged it.
func (s *Store) ImportTree(ctx context.Context, prefix string, specs []storage.ImportTreeSpec) (storage.ImportTreeResult, error) {
	creates, err := storage.ParseImportTreeFile(specs)
	if err != nil {
		return storage.ImportTreeResult{}, err
	}
	idMap := make(map[string]string, len(creates))
	created := make([]storage.IDMapping, 0, len(creates))
	createdIDs := make([]string, 0, len(creates))

	for _, create := range creates {
		issue, err := s.CreateIssue(ctx, create.Input(idMap[create.Parent], prefix))
		if err != nil {
			leaked := s.rollbackCreatedIssues(ctx, createdIDs)
			return storage.ImportTreeResult{}, fmt.Errorf("import: create %q: %w (rollback leaked %d: %s)", create.LocalID, err, len(leaked), strings.Join(leaked, ","))
		}
		idMap[create.LocalID] = issue.ID
		created = append(created, storage.NewIDMapping(create.LocalID, issue.ID))
		createdIDs = append(createdIDs, issue.ID)
	}
	for i, create := range creates {
		for _, dep := range create.BlockedBy {
			if _, err := s.AddRelation(ctx, storage.AddRelationInput{SrcID: createdIDs[i], DstID: idMap[dep], Type: model.RelBlockedBy, CreatedBy: "links"}); err != nil {
				leaked := s.rollbackCreatedIssues(ctx, createdIDs)
				return storage.ImportTreeResult{}, fmt.Errorf("import: blocked_by %q -> %q: %w (rollback leaked %d: %s)", create.LocalID, dep, err, len(leaked), strings.Join(leaked, ","))
			}
		}
	}
	return storage.ImportTreeResult{Created: created}, nil
}

// rollbackCreatedIssues best-effort deletes issues already created by
// transitioning each to "deleted". Returns the IDs that could not be cleaned
// up so the surfaced error can name them; the caller still returns the
// original error unchanged. Shared by ImportTree and BulkApply — both create
// issues in a loop and must unwind the same way on a later failure.
// [LAW:one-source-of-truth]
func (s *Store) rollbackCreatedIssues(ctx context.Context, createdIDs []string) []string {
	leaked := []string{}
	for _, realID := range createdIDs {
		if _, err := s.Apply(ctx, realID, storage.Change{Action: model.Delete{}, Actor: "links", Reason: "import rollback"}); err != nil {
			leaked = append(leaked, realID)
		}
	}
	return leaked
}
