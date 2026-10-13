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
// owns ID resolution; the create order is the one storage.ImportTreeWriteOrder
// hands back once it has judged the file.
func (s *Store) ImportTree(ctx context.Context, prefix string, specs []storage.ImportTreeSpec) (storage.ImportTreeResult, error) {
	order, err := storage.ImportTreeWriteOrder(specs)
	if err != nil {
		return storage.ImportTreeResult{}, err
	}
	idMap := make(map[string]string, len(specs))
	created := make([]storage.IDMapping, 0, len(specs))
	createdIDs := make([]string, 0, len(specs))

	for _, idx := range order {
		spec := specs[idx]
		parentID := ""
		if spec.Parent != "" {
			parentID = idMap[spec.Parent]
		}
		// validateImportTreeSpecs already gated membership before the first
		// create; parsing again here is the same single gate producing the
		// typed value, not a second definition of validity.
		issueType, err := model.ParseIssueType(spec.IssueType)
		if err != nil {
			return storage.ImportTreeResult{}, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		priority, err := model.ParsePriority(spec.Priority)
		if err != nil {
			return storage.ImportTreeResult{}, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		issue, err := s.CreateIssue(ctx, storage.CreateIssueInput{
			Title:       spec.Title,
			Description: spec.Description,
			Prompt:      spec.Prompt,
			IssueType:   issueType,
			Topic:       spec.Topic,
			Priority:    priority,
			Assignee:    spec.Assignee,
			Labels:      spec.Labels,
			ParentID:    parentID,
			Prefix:      prefix,
		})
		if err != nil {
			leaked := s.rollbackCreatedIssues(ctx, createdIDs)
			return storage.ImportTreeResult{}, fmt.Errorf("import: create %q: %w (rollback leaked %d: %s)", spec.LocalID, err, len(leaked), strings.Join(leaked, ","))
		}
		idMap[spec.LocalID] = issue.ID
		created = append(created, storage.NewIDMapping(spec.LocalID, issue.ID))
		createdIDs = append(createdIDs, issue.ID)
	}
	for _, spec := range specs {
		for _, dep := range spec.BlockedBy {
			srcID := idMap[spec.LocalID]
			dstID := idMap[dep]
			if _, err := s.AddRelation(ctx, storage.AddRelationInput{SrcID: srcID, DstID: dstID, Type: model.RelBlockedBy, CreatedBy: "links"}); err != nil {
				leaked := s.rollbackCreatedIssues(ctx, createdIDs)
				return storage.ImportTreeResult{}, fmt.Errorf("import: blocked_by %q -> %q: %w (rollback leaked %d: %s)", spec.LocalID, dep, err, len(leaked), strings.Join(leaked, ","))
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
