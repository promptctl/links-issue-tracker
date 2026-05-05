package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/model"
)

// ImportTreeSpec is a single record in a declarative tree-import file. LocalID
// is opaque — it's used inside the spec to wire Parent and DependsOn refs and
// is replaced with the generated lit issue ID at import time.
type ImportTreeSpec struct {
	LocalID     string   `json:"local_id" yaml:"local_id"`
	Title       string   `json:"title" yaml:"title"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
	Prompt      string   `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	IssueType   string   `json:"type" yaml:"type"`
	Topic       string   `json:"topic" yaml:"topic"`
	Priority    int      `json:"priority" yaml:"priority"`
	Assignee    string   `json:"assignee,omitempty" yaml:"assignee,omitempty"`
	Labels      []string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Parent      string   `json:"parent,omitempty" yaml:"parent,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`
}

// ImportTreeResult reports the local-ID → real-issue-ID mapping produced by a
// successful import.
type ImportTreeResult struct {
	IDMap map[string]string `json:"id_map"`
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

	for _, idx := range order {
		spec := specs[idx]
		parentID := ""
		if spec.Parent != "" {
			parentID = idMap[spec.Parent]
		}
		issue, err := s.CreateIssue(ctx, CreateIssueInput{
			Title:       spec.Title,
			Description: spec.Description,
			Prompt:      spec.Prompt,
			IssueType:   spec.IssueType,
			Topic:       spec.Topic,
			Priority:    spec.Priority,
			Assignee:    spec.Assignee,
			Labels:      spec.Labels,
			ParentID:    parentID,
			Prefix:      prefix,
		})
		if err != nil {
			leaked := s.rollbackImportTreePartial(ctx, idMap)
			return ImportTreeResult{}, fmt.Errorf("import: create %q: %w (rollback leaked %d: %s)", spec.LocalID, err, len(leaked), strings.Join(leaked, ","))
		}
		idMap[spec.LocalID] = issue.ID
	}
	for _, spec := range specs {
		for _, dep := range spec.DependsOn {
			srcID := idMap[spec.LocalID]
			dstID := idMap[dep]
			// blocks convention in the store: src is dependent, dst is dependency.
			// spec says "srcID depends_on dstID", so we pass src as dependent.
			if _, err := s.AddRelation(ctx, AddRelationInput{SrcID: srcID, DstID: dstID, Type: "blocks", CreatedBy: "links"}); err != nil {
				leaked := s.rollbackImportTreePartial(ctx, idMap)
				return ImportTreeResult{}, fmt.Errorf("import: depends_on %q -> %q: %w (rollback leaked %d: %s)", spec.LocalID, dep, err, len(leaked), strings.Join(leaked, ","))
			}
		}
	}
	return ImportTreeResult{IDMap: idMap}, nil
}

// rollbackImportTreePartial best-effort deletes issues already created by
// transitioning each to "deleted". Returns the IDs that could not be cleaned
// up so the surfaced error can name them; the caller still returns the
// original error unchanged.
func (s *Store) rollbackImportTreePartial(ctx context.Context, idMap map[string]string) []string {
	leaked := []string{}
	for _, realID := range idMap {
		if _, err := s.TransitionIssue(ctx, TransitionIssueInput{IssueID: realID, Action: "delete", CreatedBy: "links", Reason: "import rollback"}); err != nil {
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
		if !model.IsValidIssueType(spec.IssueType) {
			return fmt.Errorf("import: spec %q has invalid type %q", spec.LocalID, spec.IssueType)
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
	indexByLocal := make(map[string]int, len(specs))
	for i, spec := range specs {
		indexByLocal[spec.LocalID] = i
	}
	const (
		stateUnvisited = 0
		stateVisiting  = 1
		stateDone      = 2
	)
	state := make([]int, len(specs))
	order := make([]int, 0, len(specs))

	var visit func(i int) error
	visit = func(i int) error {
		switch state[i] {
		case stateDone:
			return nil
		case stateVisiting:
			return fmt.Errorf("import: cycle detected involving %q", specs[i].LocalID)
		}
		state[i] = stateVisiting
		spec := specs[i]
		if spec.Parent != "" {
			if err := visit(indexByLocal[spec.Parent]); err != nil {
				return err
			}
		}
		for _, dep := range spec.DependsOn {
			if err := visit(indexByLocal[dep]); err != nil {
				return err
			}
		}
		state[i] = stateDone
		order = append(order, i)
		return nil
	}
	for i := range specs {
		if err := visit(i); err != nil {
			return nil, err
		}
	}
	return order, nil
}
