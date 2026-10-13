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
// owns ID resolution + topological create order.
func (s *Store) ImportTree(ctx context.Context, prefix string, specs []storage.ImportTreeSpec) (storage.ImportTreeResult, error) {
	if err := storage.ValidateImportTreeSpecs(specs); err != nil {
		return storage.ImportTreeResult{}, err
	}
	order, err := topoSortImportSpecs(specs)
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

// topoSortImportSpecs returns indices of specs in an order such that for
// every (i, j) where j depends on i (via Parent or BlockedBy), i appears
// first. Cycle in the graph is rejected with an error.
func topoSortImportSpecs(specs []storage.ImportTreeSpec) ([]int, error) {
	localID := make([]string, len(specs))
	parent := make([]string, len(specs))
	dependsOn := make([][]string, len(specs))
	for i, spec := range specs {
		localID[i] = spec.LocalID
		parent[i] = spec.Parent
		dependsOn[i] = spec.BlockedBy
	}
	order, err := topoSortLocalGraph(localID, parent, dependsOn)
	if err != nil {
		return nil, fmt.Errorf("import: %w", err)
	}
	return order, nil
}

// topoSortLocalGraph orders the n nodes named by localID so that every node
// appears after every other node it names via parent or dependsOn. A
// reference that does not match any localID entry is not a graph edge — it
// resolves outside this batch, and the caller (CreateIssue/AddRelation for a
// real ID, or upstream validation for a required internal one) is
// responsible for treating that as it needs to. Entries with an empty
// localID cannot be referenced (an empty string is never treated as a name)
// and always sort by encounter order relative to their own edges. Cycles
// among graph-internal edges are rejected. Shared by ImportTree's tree spec
// (every reference is internal, validated before this runs) and BulkApply's
// create graph (references may be internal or external — external refs are
// exactly the ones this function ignores). [LAW:one-source-of-truth]
func topoSortLocalGraph(localID, parent []string, dependsOn [][]string) ([]int, error) {
	indexByLocal := make(map[string]int, len(localID))
	for i, id := range localID {
		if id == "" {
			continue
		}
		indexByLocal[id] = i
	}
	const (
		stateUnvisited = 0
		stateVisiting  = 1
		stateDone      = 2
	)
	state := make([]int, len(localID))
	order := make([]int, 0, len(localID))

	var visit func(i int) error
	visit = func(i int) error {
		switch state[i] {
		case stateDone:
			return nil
		case stateVisiting:
			return fmt.Errorf("cycle detected involving %q", localID[i])
		}
		state[i] = stateVisiting
		if j, ok := indexByLocal[parent[i]]; parent[i] != "" && ok {
			if err := visit(j); err != nil {
				return err
			}
		}
		for _, dep := range dependsOn[i] {
			if j, ok := indexByLocal[dep]; ok {
				if err := visit(j); err != nil {
					return err
				}
			}
		}
		state[i] = stateDone
		order = append(order, i)
		return nil
	}
	for i := range localID {
		if err := visit(i); err != nil {
			return nil, err
		}
	}
	return order, nil
}
