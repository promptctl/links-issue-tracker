package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// ParseImportTreeSpecs is the deserialization trust boundary for tree-import
// files: raw bytes in, specs out. It rejects any field the spec schema does
// not name and any trailing data after the array, so a drifted or typo'd spec
// fails loudly here instead of silently losing the unrecognized data downstream.
//
// [LAW:single-enforcer] The store owns the ImportTreeSpec schema, so the store
// owns its deserialization — the CLI hands bytes here rather than running its
// own permissive decode.
// [LAW:no-silent-failure] DisallowUnknownFields + trailing-data check make
// the parse total: every byte stream that is not exactly one array of
// known-field specs is an explicit error.
func ParseImportTreeSpecs(data []byte) ([]ImportTreeSpec, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var specs []ImportTreeSpec
	if err := dec.Decode(&specs); err != nil {
		return nil, fmt.Errorf("import: parse spec: %w", err)
	}
	if dec.More() {
		return nil, errors.New("import: unexpected trailing data after spec array")
	}
	return specs, nil
}

// ImportTree validates and creates a tree of issues described by specs in
// dependency order: parents and DependsOn referents are created first, then
// their children/dependents. Parent relations are wired by CreateIssue itself
// (via ParentID); blocks edges are added in a second pass.
//
// On any failure mid-import, ImportTree best-effort rolls back already-created
// issues by transitioning them to deleted. Atomicity is therefore best-effort:
// if rollback itself fails, partial state remains and the surfaced error
// names every step that's left dangling. Run `lit doctor` after a failed
// import to detect orphans.
//
// [LAW:single-enforcer] Atomic tree import is the one shared boundary that
// owns ID resolution + topological create order.
func (s *Store) ImportTree(ctx context.Context, prefix string, specs []ImportTreeSpec) (ImportTreeResult, error) {
	if err := validateImportTreeSpecs(specs); err != nil {
		return ImportTreeResult{}, err
	}
	order, err := topoSortImportSpecs(specs)
	if err != nil {
		return ImportTreeResult{}, err
	}
	idMap := make(map[string]string, len(specs))
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
			return ImportTreeResult{}, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		priority, err := model.ParsePriority(spec.Priority)
		if err != nil {
			return ImportTreeResult{}, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		issue, err := s.CreateIssue(ctx, CreateIssueInput{
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
			return ImportTreeResult{}, fmt.Errorf("import: create %q: %w (rollback leaked %d: %s)", spec.LocalID, err, len(leaked), strings.Join(leaked, ","))
		}
		idMap[spec.LocalID] = issue.ID
		createdIDs = append(createdIDs, issue.ID)
	}
	for _, spec := range specs {
		for _, dep := range spec.DependsOn {
			srcID := idMap[spec.LocalID]
			dstID := idMap[dep]
			// blocks convention in the store: src is dependent, dst is dependency.
			// spec says "srcID depends_on dstID", so we pass src as dependent.
			if _, err := s.AddRelation(ctx, AddRelationInput{SrcID: srcID, DstID: dstID, Type: "blocks", CreatedBy: "links"}); err != nil {
				leaked := s.rollbackCreatedIssues(ctx, createdIDs)
				return ImportTreeResult{}, fmt.Errorf("import: depends_on %q -> %q: %w (rollback leaked %d: %s)", spec.LocalID, dep, err, len(leaked), strings.Join(leaked, ","))
			}
		}
	}
	return ImportTreeResult{IDMap: idMap}, nil
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
		if _, err := s.Apply(ctx, realID, Change{Action: model.Delete{}, Actor: "links", Reason: "import rollback"}); err != nil {
			leaked = append(leaked, realID)
		}
	}
	return leaked
}

func validateImportTreeSpecs(specs []ImportTreeSpec) error {
	if len(specs) == 0 {
		return errors.New("import: no issues in input")
	}
	seen := make(map[string]struct{}, len(specs))
	for i, spec := range specs {
		if strings.TrimSpace(spec.LocalID) == "" {
			return fmt.Errorf("import: spec %d missing local_id", i)
		}
		if spec.LocalID != strings.TrimSpace(spec.LocalID) {
			return fmt.Errorf("import: spec %d local_id %q has surrounding whitespace", i, spec.LocalID)
		}
		if strings.TrimSpace(spec.Title) == "" {
			return fmt.Errorf("import: spec %q missing title", spec.LocalID)
		}
		// [LAW:single-enforcer] Membership comes from the one ParseIssueType
		// gate; specs are agent-authored input, so they parse strictly here —
		// before any issue is created — rather than relying on the salvage
		// convention.
		if _, err := model.ParseIssueType(spec.IssueType); err != nil {
			return fmt.Errorf("import: spec %q has invalid type %q", spec.LocalID, spec.IssueType)
		}
		if _, err := model.ParsePriority(spec.Priority); err != nil {
			return fmt.Errorf("import: spec %q has invalid priority %d", spec.LocalID, spec.Priority)
		}
		if _, dup := seen[spec.LocalID]; dup {
			return fmt.Errorf("import: duplicate local_id %q", spec.LocalID)
		}
		seen[spec.LocalID] = struct{}{}
	}
	for _, spec := range specs {
		if spec.Parent != "" {
			if spec.Parent != strings.TrimSpace(spec.Parent) {
				return fmt.Errorf("import: spec %q parent %q has surrounding whitespace", spec.LocalID, spec.Parent)
			}
			if _, ok := seen[spec.Parent]; !ok {
				return fmt.Errorf("import: spec %q references missing parent %q", spec.LocalID, spec.Parent)
			}
		}
		for _, dep := range spec.DependsOn {
			if dep != strings.TrimSpace(dep) {
				return fmt.Errorf("import: spec %q depends_on entry %q has surrounding whitespace", spec.LocalID, dep)
			}
			if _, ok := seen[dep]; !ok {
				return fmt.Errorf("import: spec %q references missing depends_on %q", spec.LocalID, dep)
			}
			if dep == spec.LocalID {
				return fmt.Errorf("import: spec %q cannot depend on itself", spec.LocalID)
			}
		}
	}
	return nil
}

// topoSortImportSpecs returns indices of specs in an order such that for
// every (i, j) where j depends on i (via Parent or DependsOn), i appears
// first. Cycle in the graph is rejected with an error.
func topoSortImportSpecs(specs []ImportTreeSpec) ([]int, error) {
	localID := make([]string, len(specs))
	parent := make([]string, len(specs))
	dependsOn := make([][]string, len(specs))
	for i, spec := range specs {
		localID[i] = spec.LocalID
		parent[i] = spec.Parent
		dependsOn[i] = spec.DependsOn
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
