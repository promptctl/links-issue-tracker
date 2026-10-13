package storage

import (
	"errors"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The two file imports — a tree file and a bulk file — are gated here, once,
// before either engine writes anything. An import creates one issue at a
// time and compensates a later refusal by deleting what already landed, so a
// file the store would refuse at its third document is refused whole, with
// nothing written, by judging up front everything that can be judged without
// a read: every reference names a document in the file, every field parses
// by the gate a create parses by, and the hierarchy the file describes obeys
// containment (RejectLeafWithoutParent, RejectParentThatIsNotAContainer). A
// parent named by a real id is judged by the create that reads it. Both
// engines call these gates, so a file is accepted or refused the same way
// whichever engine opens it. [LAW:single-enforcer] [LAW:one-source-of-truth]

// ValidateImportTreeSpecs gates a tree-import file.
func ValidateImportTreeSpecs(specs []ImportTreeSpec) error {
	if len(specs) == 0 {
		return errors.New("import: no issues in input")
	}
	types := make(map[string]model.IssueType, len(specs))
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
		// Membership comes from the one ParseIssueType gate; specs are
		// agent-authored input, so they parse strictly here rather than
		// relying on the salvage convention. The parsed type is kept: the
		// containment checks below judge by it. [LAW:parse-dont-validate]
		issueType, err := model.ParseIssueType(spec.IssueType)
		if err != nil {
			return fmt.Errorf("import: spec %q has invalid type %q", spec.LocalID, spec.IssueType)
		}
		if _, err := model.ParsePriority(spec.Priority); err != nil {
			return fmt.Errorf("import: spec %q has invalid priority %d", spec.LocalID, spec.Priority)
		}
		if _, dup := types[spec.LocalID]; dup {
			return fmt.Errorf("import: duplicate local_id %q", spec.LocalID)
		}
		types[spec.LocalID] = issueType
	}
	for _, spec := range specs {
		if spec.Parent != "" {
			if spec.Parent != strings.TrimSpace(spec.Parent) {
				return fmt.Errorf("import: spec %q parent %q has surrounding whitespace", spec.LocalID, spec.Parent)
			}
			parentType, ok := types[spec.Parent]
			if !ok {
				return fmt.Errorf("import: spec %q references missing parent %q", spec.LocalID, spec.Parent)
			}
			if err := RejectParentThatIsNotAContainer(spec.Parent, parentType); err != nil {
				return fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
			}
		}
		if err := RejectLeafWithoutParent(types[spec.LocalID], spec.Parent); err != nil {
			return fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		for _, dep := range spec.DependsOn {
			if dep != strings.TrimSpace(dep) {
				return fmt.Errorf("import: spec %q depends_on entry %q has surrounding whitespace", spec.LocalID, dep)
			}
			if _, ok := types[dep]; !ok {
				return fmt.Errorf("import: spec %q references missing depends_on %q", spec.LocalID, dep)
			}
			if dep == spec.LocalID {
				return fmt.Errorf("import: spec %q cannot depend on itself", spec.LocalID)
			}
		}
	}
	return nil
}

// ValidateBulkSpecs gates a bulk file: every document is a create or an
// update by whether it names an id, and each shape admits exactly the fields
// its verb owns. A create document's parent may be a local_id in the file or
// a real id; only the first can be judged here.
func ValidateBulkSpecs(specs []BulkIssueSpec) error {
	if len(specs) == 0 {
		return errors.New("bulk: no issues in input")
	}
	seenID := make(map[string]struct{}, len(specs))
	localTypes := make(map[string]model.IssueType, len(specs))
	createTypes := make(map[int]model.IssueType, len(specs))
	for i, spec := range specs {
		if spec.ID != strings.TrimSpace(spec.ID) {
			return fmt.Errorf("bulk: doc %d id %q has surrounding whitespace", i, spec.ID)
		}
		if spec.LocalID != strings.TrimSpace(spec.LocalID) {
			return fmt.Errorf("bulk: doc %d local_id %q has surrounding whitespace", i, spec.LocalID)
		}
		if spec.Parent != strings.TrimSpace(spec.Parent) {
			return fmt.Errorf("bulk: doc %d parent %q has surrounding whitespace", i, spec.Parent)
		}
		for _, dep := range spec.DependsOn {
			if dep != strings.TrimSpace(dep) {
				return fmt.Errorf("bulk: doc %d depends_on entry %q has surrounding whitespace", i, dep)
			}
			if spec.LocalID != "" && dep == spec.LocalID {
				return fmt.Errorf("bulk: doc %d (local_id %q) cannot depend on itself", i, spec.LocalID)
			}
		}

		if spec.ID != "" {
			if err := validateBulkUpdateDoc(i, spec); err != nil {
				return err
			}
			if _, dup := seenID[spec.ID]; dup {
				return fmt.Errorf("bulk: duplicate id %q", spec.ID)
			}
			seenID[spec.ID] = struct{}{}
			continue
		}

		issueType, err := validateBulkCreateDoc(i, spec)
		if err != nil {
			return err
		}
		createTypes[i] = issueType
		if spec.LocalID != "" {
			if _, dup := localTypes[spec.LocalID]; dup {
				return fmt.Errorf("bulk: duplicate local_id %q", spec.LocalID)
			}
			localTypes[spec.LocalID] = issueType
		}
	}
	for i, spec := range specs {
		issueType, creates := createTypes[i]
		if !creates {
			continue
		}
		if err := RejectLeafWithoutParent(issueType, spec.Parent); err != nil {
			return fmt.Errorf("bulk: doc %d: %w", i, err)
		}
		if parentType, inFile := localTypes[spec.Parent]; inFile {
			if err := RejectParentThatIsNotAContainer(spec.Parent, parentType); err != nil {
				return fmt.Errorf("bulk: doc %d: %w", i, err)
			}
		}
	}
	return nil
}

// validateBulkCreateDoc admits a create document and returns its parsed type,
// the one fact the containment checks need from it.
func validateBulkCreateDoc(i int, spec BulkIssueSpec) (model.IssueType, error) {
	if spec.Title == nil || strings.TrimSpace(*spec.Title) == "" {
		return "", fmt.Errorf("bulk: doc %d missing title", i)
	}
	if spec.Topic == nil || strings.TrimSpace(*spec.Topic) == "" {
		return "", fmt.Errorf("bulk: doc %d missing topic", i)
	}
	if spec.IssueType == nil {
		return "", fmt.Errorf("bulk: doc %d missing type", i)
	}
	issueType, err := model.ParseIssueType(*spec.IssueType)
	if err != nil {
		return "", fmt.Errorf("bulk: doc %d has invalid type %q", i, *spec.IssueType)
	}
	if spec.Priority != nil {
		if _, err := model.ParsePriority(*spec.Priority); err != nil {
			return "", fmt.Errorf("bulk: doc %d has invalid priority %d", i, *spec.Priority)
		}
	}
	if spec.Reason != "" {
		return "", fmt.Errorf("bulk: doc %d sets reason without id (reason only applies to updates)", i)
	}
	return issueType, nil
}

// validateBulkUpdateDoc refuses the fields an update document has no business
// setting. Each one is somebody else's verb — topic is immutable, reparenting
// and dependency wiring are their own commands — so a document that states
// one is asking for something this path would silently not do.
func validateBulkUpdateDoc(i int, spec BulkIssueSpec) error {
	if spec.LocalID != "" {
		return fmt.Errorf("bulk: doc %d (id %q) sets local_id; local_id only applies to new tickets", i, spec.ID)
	}
	if spec.Topic != nil {
		return fmt.Errorf("bulk: doc %d (id %q) sets topic; topic is immutable and update cannot change it", i, spec.ID)
	}
	if spec.Parent != "" {
		return fmt.Errorf("bulk: doc %d (id %q) sets parent; reparent with `lit parent set` instead", i, spec.ID)
	}
	if len(spec.DependsOn) > 0 {
		return fmt.Errorf("bulk: doc %d (id %q) sets depends_on; wire dependencies with `lit dep add` instead", i, spec.ID)
	}
	if spec.IssueType != nil {
		if _, err := model.ParseIssueType(*spec.IssueType); err != nil {
			return fmt.Errorf("bulk: doc %d (id %q) has invalid type %q", i, spec.ID, *spec.IssueType)
		}
	}
	if spec.Priority != nil {
		if _, err := model.ParsePriority(*spec.Priority); err != nil {
			return fmt.Errorf("bulk: doc %d (id %q) has invalid priority %d", i, spec.ID, *spec.Priority)
		}
	}
	if !bulkUpdateHasField(spec) {
		return fmt.Errorf("bulk: doc %d (id %q) has no fields to update", i, spec.ID)
	}
	return nil
}

func bulkUpdateHasField(spec BulkIssueSpec) bool {
	return spec.Title != nil || spec.Description != nil || spec.Prompt != nil ||
		spec.IssueType != nil || spec.Priority != nil || spec.Assignee != nil ||
		spec.Labels != nil || spec.Lane != nil
}
