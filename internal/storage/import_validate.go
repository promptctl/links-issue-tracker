package storage

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The two file imports — a tree file and a bulk file — are gated here, once,
// before either engine writes anything. An import creates one issue at a
// time and compensates a later refusal by deleting what already landed, so a
// file the store would refuse at its third document is refused whole, with
// nothing written, by judging up front everything that can be judged without
// a read: every reference names a document in the file, every field parses
// by the gate a create parses by, the references form no cycle, the
// hierarchy the file describes obeys containment (RejectLeafWithoutParent,
// RejectParentThatIsNotAContainer), and no blocked_by runs along that
// hierarchy (RejectBlocksAlongHierarchy). A parent or blocked_by named by a
// real id is judged by the write that reads it. Both engines take their
// write order from these gates, so a file is accepted or refused the same
// way whichever engine opens it, and no engine can write a file the gate has
// not judged.
// [LAW:single-enforcer] [LAW:one-source-of-truth] [LAW:parse-dont-validate]
//
// Every refusal here is a model.ValidationError: the file is what is wrong,
// so the same file is refused the same way on every run.

// ImportTreeWriteOrder gates a tree-import file and returns the order its
// documents are written in.
func ImportTreeWriteOrder(specs []ImportTreeSpec) ([]int, error) {
	if err := validateImportTreeSpecs(specs); err != nil {
		return nil, err
	}
	return judgeReferences("import", resolveFileReferences(specs, func(_ int, spec ImportTreeSpec) fileDocument {
		return fileDocument{name: fmt.Sprintf("spec %q", spec.LocalID), localID: spec.LocalID, parent: spec.Parent, blockedBy: spec.BlockedBy}
	}))
}

// BulkWriteOrder gates a bulk file and returns the order its documents are
// written in.
func BulkWriteOrder(specs []BulkIssueSpec) ([]int, error) {
	if err := validateBulkSpecs(specs); err != nil {
		return nil, err
	}
	return judgeReferences("bulk", resolveFileReferences(specs, func(i int, spec BulkIssueSpec) fileDocument {
		return fileDocument{name: fmt.Sprintf("doc %d", i), localID: spec.LocalID, parent: spec.Parent, blockedBy: spec.BlockedBy}
	}))
}

// fileDocument is what the reference rules read from one document: its
// references, and the name a refusal calls it by, in its format's own words.
type fileDocument struct {
	name, localID, parent string
	blockedBy             []string
}

// fileReferences is the graph a file draws among its own documents, indexed
// by document position, since a bulk create need not carry a local_id. A
// reference matching no local_id in the file is not an edge: it names a real
// issue, and the write that receives it judges it.
type fileReferences struct {
	docs      []fileDocument
	parent    map[int]int // document -> its parent, for a parent in the file
	blockedBy [][]int     // document -> the documents in the file it is blocked by
}

func resolveFileReferences[S any](specs []S, documentOf func(int, S) fileDocument) fileReferences {
	refs := fileReferences{docs: make([]fileDocument, len(specs)), parent: make(map[int]int, len(specs)), blockedBy: make([][]int, len(specs))}
	indexByLocal := make(map[string]int, len(specs))
	for i, spec := range specs {
		refs.docs[i] = documentOf(i, spec)
		if localID := refs.docs[i].localID; localID != "" {
			indexByLocal[localID] = i
		}
	}
	for i, doc := range refs.docs {
		if j, inFile := indexByLocal[doc.parent]; inFile {
			refs.parent[i] = j
		}
		for _, dep := range doc.blockedBy {
			if j, inFile := indexByLocal[dep]; inFile {
				refs.blockedBy[i] = append(refs.blockedBy[i], j)
			}
		}
	}
	return refs
}

// judgeReferences refuses a file whose references break a rule over the
// graph they draw, and otherwise returns the order its documents are written
// in. The hierarchy rule is judged first: an epic blocked by something under
// it is also a cycle in write order, and the rule's own refusal is the one
// that names the fix.
func judgeReferences(format string, refs fileReferences) ([]int, error) {
	if err := rejectBlocksAlongFileHierarchy(format, refs); err != nil {
		return nil, err
	}
	return writeOrder(format, refs)
}

// writeOrder is a depth-first topological sort of a file's documents over
// their parent and blocked_by references, so each document is written after
// every document it names. A cycle among the file's own references is
// refused.
func writeOrder(format string, refs fileReferences) ([]int, error) {
	const (
		unvisited = iota
		visiting
		done
	)
	state := make([]int, len(refs.docs))
	order := make([]int, 0, len(refs.docs))
	var visit func(int) error
	visit = func(i int) error {
		switch state[i] {
		case done:
			return nil
		case visiting:
			return model.ValidationError{Message: fmt.Sprintf("%s: cycle detected involving %q", format, refs.docs[i].localID)}
		}
		state[i] = visiting
		if p, inFile := refs.parent[i]; inFile {
			if err := visit(p); err != nil {
				return err
			}
		}
		for _, j := range refs.blockedBy[i] {
			if err := visit(j); err != nil {
				return err
			}
		}
		state[i] = done
		order = append(order, i)
		return nil
	}
	for i := range refs.docs {
		if err := visit(i); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// rejectBlocksAlongFileHierarchy holds the file's own blocked_by edges to
// the rule every blocked-by write meets, over the hierarchy the file
// describes, so a document blocked by an epic it sits under in the file, or
// an epic blocked by a document under it, is refused before the first
// create rather than at its edge, after the last. Every parent in the file
// is an epic by now — the validators refused any other — so every parent
// edge is one the rule climbs. An edge whose climb would leave the file
// through a real id is the write's to judge. [LAW:single-enforcer]
func rejectBlocksAlongFileHierarchy(format string, refs fileReferences) error {
	node := strconv.Itoa
	epicParentOf := make(map[string][]string, len(refs.parent))
	for i, p := range refs.parent {
		epicParentOf[node(i)] = []string{node(p)}
	}
	for i, deps := range refs.blockedBy {
		for _, j := range deps {
			if err := RejectBlocksAlongHierarchy(epicParentOf, node(i), node(j)); err != nil {
				return fmt.Errorf("%s: %s blocked_by %q: %w", format, refs.docs[i].name, refs.docs[j].localID, err)
			}
		}
	}
	return nil
}

// validateImportTreeSpecs judges a tree-import file by every rule but those
// over the graph its references draw, which judgeReferences makes.
func validateImportTreeSpecs(specs []ImportTreeSpec) error {
	if len(specs) == 0 {
		return model.ValidationError{Message: "import: no issues in input"}
	}
	types := make(map[string]model.IssueType, len(specs))
	for i, spec := range specs {
		if strings.TrimSpace(spec.LocalID) == "" {
			return model.ValidationError{Message: fmt.Sprintf("import: spec %d missing local_id", i)}
		}
		if spec.LocalID != strings.TrimSpace(spec.LocalID) {
			return model.ValidationError{Message: fmt.Sprintf("import: spec %d local_id %q has surrounding whitespace", i, spec.LocalID)}
		}
		if strings.TrimSpace(spec.Title) == "" {
			return model.ValidationError{Message: fmt.Sprintf("import: spec %q missing title", spec.LocalID)}
		}
		// Topic, labels, and assignee parse by the parsers the create runs,
		// so a file the create would refuse is refused here, before anything
		// lands. [LAW:single-enforcer]
		if _, err := issueid.NormalizeTopicForCreate(spec.Topic); err != nil {
			return fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		if _, err := model.CanonicalizeLabels(spec.Labels); err != nil {
			return fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		if _, err := model.NormalizeAssignee(spec.Assignee); err != nil {
			return fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		// Membership comes from the one ParseIssueType gate; specs are
		// agent-authored input, so they parse strictly here rather than
		// relying on the salvage convention. The parsed type is kept: the
		// containment checks below judge by it. [LAW:parse-dont-validate]
		issueType, err := model.ParseIssueType(spec.IssueType)
		if err != nil {
			return model.ValidationError{Message: fmt.Sprintf("import: spec %q has invalid type %q", spec.LocalID, spec.IssueType)}
		}
		if _, err := model.ParsePriority(spec.Priority); err != nil {
			return model.ValidationError{Message: fmt.Sprintf("import: spec %q has invalid priority %d", spec.LocalID, spec.Priority)}
		}
		if _, dup := types[spec.LocalID]; dup {
			return model.ValidationError{Message: fmt.Sprintf("import: duplicate local_id %q", spec.LocalID)}
		}
		types[spec.LocalID] = issueType
	}
	for _, spec := range specs {
		if spec.Parent != "" {
			if spec.Parent != strings.TrimSpace(spec.Parent) {
				return model.ValidationError{Message: fmt.Sprintf("import: spec %q parent %q has surrounding whitespace", spec.LocalID, spec.Parent)}
			}
			parentType, ok := types[spec.Parent]
			if !ok {
				return model.ValidationError{Message: fmt.Sprintf("import: spec %q references missing parent %q", spec.LocalID, spec.Parent)}
			}
			if err := RejectParentThatIsNotAContainer(spec.Parent, parentType); err != nil {
				return fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
			}
		}
		if err := RejectLeafWithoutParent(types[spec.LocalID], spec.Parent); err != nil {
			return fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		if err := rejectRepeatedBlockedBy(spec.BlockedBy); err != nil {
			return fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		for _, dep := range spec.BlockedBy {
			if dep != strings.TrimSpace(dep) {
				return model.ValidationError{Message: fmt.Sprintf("import: spec %q blocked_by entry %q has surrounding whitespace", spec.LocalID, dep)}
			}
			if _, ok := types[dep]; !ok {
				return model.ValidationError{Message: fmt.Sprintf("import: spec %q references missing blocked_by %q", spec.LocalID, dep)}
			}
			if dep == spec.LocalID {
				return model.ValidationError{Message: fmt.Sprintf("import: spec %q cannot depend on itself", spec.LocalID)}
			}
		}
	}
	return nil
}

// validateBulkSpecs judges a bulk file by every rule but those over the
// graph its references draw, which judgeReferences makes:
// every document is a create or an update by whether it names an id, and each
// shape admits exactly the fields its verb owns. A create document's parent
// may be a local_id in the file or a real id; only the first can be judged
// here.
func validateBulkSpecs(specs []BulkIssueSpec) error {
	if len(specs) == 0 {
		return model.ValidationError{Message: "bulk: no issues in input"}
	}
	seenID := make(map[string]struct{}, len(specs))
	localTypes := make(map[string]model.IssueType, len(specs))
	createTypes := make(map[int]model.IssueType, len(specs))
	for i, spec := range specs {
		if spec.ID != strings.TrimSpace(spec.ID) {
			return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d id %q has surrounding whitespace", i, spec.ID)}
		}
		if spec.LocalID != strings.TrimSpace(spec.LocalID) {
			return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d local_id %q has surrounding whitespace", i, spec.LocalID)}
		}
		if spec.Parent != strings.TrimSpace(spec.Parent) {
			return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d parent %q has surrounding whitespace", i, spec.Parent)}
		}
		for _, dep := range spec.BlockedBy {
			if dep != strings.TrimSpace(dep) {
				return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d blocked_by entry %q has surrounding whitespace", i, dep)}
			}
			if spec.LocalID != "" && dep == spec.LocalID {
				return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (local_id %q) cannot depend on itself", i, spec.LocalID)}
			}
		}
		// A create and an update both write labels and the assignee, by the
		// one parser either write runs. [LAW:single-enforcer]
		if spec.Labels != nil {
			if _, err := model.CanonicalizeLabels(*spec.Labels); err != nil {
				return fmt.Errorf("bulk: doc %d: %w", i, err)
			}
		}
		if spec.Assignee != nil {
			if _, err := model.NormalizeAssignee(*spec.Assignee); err != nil {
				return fmt.Errorf("bulk: doc %d: %w", i, err)
			}
		}

		if spec.ID != "" {
			if err := validateBulkUpdateDoc(i, spec); err != nil {
				return err
			}
			if _, dup := seenID[spec.ID]; dup {
				return model.ValidationError{Message: fmt.Sprintf("bulk: duplicate id %q", spec.ID)}
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
				return model.ValidationError{Message: fmt.Sprintf("bulk: duplicate local_id %q", spec.LocalID)}
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
		return "", model.ValidationError{Message: fmt.Sprintf("bulk: doc %d missing title", i)}
	}
	if spec.Topic == nil {
		return "", model.ValidationError{Message: fmt.Sprintf("bulk: doc %d missing topic", i)}
	}
	if _, err := issueid.NormalizeTopicForCreate(*spec.Topic); err != nil {
		return "", fmt.Errorf("bulk: doc %d: %w", i, err)
	}
	if spec.IssueType == nil {
		return "", model.ValidationError{Message: fmt.Sprintf("bulk: doc %d missing type", i)}
	}
	issueType, err := model.ParseIssueType(*spec.IssueType)
	if err != nil {
		return "", model.ValidationError{Message: fmt.Sprintf("bulk: doc %d has invalid type %q", i, *spec.IssueType)}
	}
	if spec.Priority != nil {
		if _, err := model.ParsePriority(*spec.Priority); err != nil {
			return "", model.ValidationError{Message: fmt.Sprintf("bulk: doc %d has invalid priority %d", i, *spec.Priority)}
		}
	}
	if spec.Reason != "" {
		return "", model.ValidationError{Message: fmt.Sprintf("bulk: doc %d sets reason without id (reason only applies to updates)", i)}
	}
	if err := rejectRepeatedBlockedBy(spec.BlockedBy); err != nil {
		return "", fmt.Errorf("bulk: doc %d: %w", i, err)
	}
	return issueType, nil
}

// rejectRepeatedBlockedBy refuses a blocked_by list that names one entry
// twice: each entry is an edge, and the store holds an edge once.
func rejectRepeatedBlockedBy(blockedBy []string) error {
	named := make(map[string]struct{}, len(blockedBy))
	for _, dep := range blockedBy {
		if _, twice := named[dep]; twice {
			return model.ValidationError{Message: fmt.Sprintf("blocked_by names %q twice", dep)}
		}
		named[dep] = struct{}{}
	}
	return nil
}

// validateBulkUpdateDoc refuses the fields an update document has no business
// setting. Each one is somebody else's verb — topic is immutable, reparenting
// and dependency wiring are their own commands — so a document that states
// one is asking for something this path would silently not do.
func validateBulkUpdateDoc(i int, spec BulkIssueSpec) error {
	if spec.LocalID != "" {
		return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) sets local_id; local_id only applies to new tickets", i, spec.ID)}
	}
	if spec.Topic != nil {
		return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) sets topic; topic is immutable and update cannot change it", i, spec.ID)}
	}
	if spec.Parent != "" {
		return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) sets parent; reparent with `lit parent set` instead", i, spec.ID)}
	}
	if len(spec.BlockedBy) > 0 {
		return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) sets blocked_by; wire dependencies with `lit dep add` instead", i, spec.ID)}
	}
	if spec.Title != nil && strings.TrimSpace(*spec.Title) == "" {
		return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) sets an empty title", i, spec.ID)}
	}
	if spec.IssueType != nil {
		if _, err := model.ParseIssueType(*spec.IssueType); err != nil {
			return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) has invalid type %q", i, spec.ID, *spec.IssueType)}
		}
	}
	if spec.Priority != nil {
		if _, err := model.ParsePriority(*spec.Priority); err != nil {
			return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) has invalid priority %d", i, spec.ID, *spec.Priority)}
		}
	}
	if !bulkUpdateHasField(spec) {
		return model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) has no fields to update", i, spec.ID)}
	}
	return nil
}

func bulkUpdateHasField(spec BulkIssueSpec) bool {
	return spec.Title != nil || spec.Description != nil || spec.Prompt != nil ||
		spec.IssueType != nil || spec.Priority != nil || spec.Assignee != nil ||
		spec.Labels != nil || spec.Lane != nil
}
