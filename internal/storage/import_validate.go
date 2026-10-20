package storage

import (
	"cmp"
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
// RejectParentThatIsNotAContainer), and no blocked_by orders work inside one
// of its epics (RejectBlocksWithinEpic), including an existing epic two of its
// documents name as their parent. That one rule reads the type of each
// existing issue the file places, which the engine reads and hands in; every
// other rule about a parent or blocked_by named by a real id is judged by the
// write that reads it. Both engines write the parsed file these gates hand back, so a
// file is accepted or refused, and written, the same way whichever engine
// opens it, and no engine can write a file the gate has not judged.
// [LAW:single-enforcer] [LAW:one-source-of-truth] [LAW:parse-dont-validate]
//
// Every refusal here is a model.ValidationError: the file is what is wrong,
// so the same file is refused the same way on every run.

// ParseImportTreeFile gates a tree-import file and returns its documents,
// parsed, in the order they are written.
func ParseImportTreeFile(specs []ImportTreeSpec) ([]FileCreate, error) {
	creates, err := parseImportTreeSpecs(specs)
	if err != nil {
		return nil, err
	}
	// A tree file's every parent is one of its own documents, so it names no
	// existing parent and there are no types to read.
	order, err := judgeReferences("import", resolveFileReferences(specs, func(i int, spec ImportTreeSpec) fileDocument {
		return fileDocument{name: fmt.Sprintf("spec %q", spec.LocalID), localID: spec.LocalID, issueType: creates[i].issue.IssueType, parent: spec.Parent, blockedBy: spec.BlockedBy}
	}), nil)
	if err != nil {
		return nil, err
	}
	return inWriteOrder(order, creates), nil
}

// ParseBulkFile gates a bulk file and returns it parsed.
// existingTypes holds the type of each existing issue among
// BulkExistingRefs(specs); an id naming no issue is absent.
//
// An update document is a document of the graph, named by its id, as a create
// is by its local_id: a create may sit under it and its own parent is an edge,
// so the graph the gate judges is the hierarchy the file writes, and a parent
// cycle the file states is refused here whatever kinds of document close it.
// Its type is the one it is stored with, which is the type the reparent write
// judges it by. [LAW:one-source-of-truth]
func ParseBulkFile(specs []BulkIssueSpec, existingTypes map[string]model.IssueType) (BulkFile, error) {
	file, creates, err := parseBulkSpecs(specs)
	if err != nil {
		return BulkFile{}, err
	}
	order, err := judgeReferences("bulk", resolveFileReferences(specs, func(i int, spec BulkIssueSpec) fileDocument {
		doc := fileDocument{name: fmt.Sprintf("doc %d", i), localID: spec.LocalID, issueType: creates[i].issue.IssueType, parent: valueOr(spec.Parent, ""), blockedBy: valueOr(spec.BlockedBy, nil)}
		if spec.ID != "" {
			doc.name = fmt.Sprintf("doc %d (id %q)", i, spec.ID)
			doc.id, doc.issueType = spec.ID, existingTypes[spec.ID]
		}
		return doc
	}), existingTypes)
	if err != nil {
		return BulkFile{}, err
	}
	// A clear's one rule needs only the stored type, which the gate holds,
	// so a clear the write would refuse is refused before any write. An
	// update cannot carry the issue across the container line, so the
	// stored type is the one the write judges. [LAW:single-enforcer]
	for _, r := range file.Reparents {
		if storedType, stored := existingTypes[r.ID]; stored && r.Parent == "" {
			if err := RejectDetachingALeaf(r.ID, storedType, 0); err != nil {
				return BulkFile{}, fmt.Errorf("bulk: parent of %q -> %q: %w", r.ID, r.Parent, err)
			}
		}
	}
	file.Creates = inWriteOrder(order, creates)
	return file, nil
}

// inWriteOrder lists a file's creates, keyed by document position, in the
// write order judgeReferences returned. An update document in that order has
// no create to list.
func inWriteOrder(order []int, creates map[int]FileCreate) []FileCreate {
	ordered := make([]FileCreate, 0, len(creates))
	for _, i := range order {
		if create, ok := creates[i]; ok {
			ordered = append(ordered, create)
		}
	}
	return ordered
}

// BulkExistingRefs lists the ids whose types an engine reads for
// ParseBulkFile: every parent a bulk file names, by local_id or by real id,
// and the id of every update document that states a parent, which the file
// places. A local_id among them names no existing issue, or is resolved to its
// document first.
func BulkExistingRefs(specs []BulkIssueSpec) []string {
	refs := make([]string, 0, len(specs))
	for _, spec := range specs {
		if spec.Parent != nil && *spec.Parent != "" {
			refs = append(refs, *spec.Parent)
		}
		if spec.Parent != nil && spec.ID != "" {
			refs = append(refs, spec.ID)
		}
	}
	return refs
}

// fileDocument is what the reference rules read from one document: the names
// a reference can call it by (a create's local_id, an update's id), its
// references, its type, and the name a refusal calls it by, in its format's
// own words.
type fileDocument struct {
	name, localID, id, parent string
	issueType                 model.IssueType
	blockedBy                 []string
}

// ref is the name a refusal quotes the document by: the one references in
// the file call it by.
func (d fileDocument) ref() string {
	if d.localID != "" {
		return d.localID
	}
	return d.id
}

// fileReferences is the graph a file draws among its own documents, indexed
// by document position, since a bulk create need not carry a local_id. A
// parent or blocked_by resolves as the engines resolve it: a local_id in the
// file first, then an update document's id, so a create blocked by an issue
// the file reparents is judged where the file puts it. A reference matching
// neither is not an edge: it names a real issue, and the write that receives
// it judges it. A parent named that way is still kept, because two documents
// under one existing epic are leaves of one epic before either is written.
type fileReferences struct {
	docs           []fileDocument
	parent         map[int]int    // document -> its parent, for a parent in the file
	existingParent map[int]string // document -> its parent, for a parent named by real id
	blockedBy      [][]int        // document -> the documents in the file it is blocked by
}

func resolveFileReferences[S any](specs []S, documentOf func(int, S) fileDocument) fileReferences {
	refs := fileReferences{docs: make([]fileDocument, len(specs)), parent: make(map[int]int, len(specs)), existingParent: make(map[int]string), blockedBy: make([][]int, len(specs))}
	indexByLocal := make(map[string]int, len(specs))
	indexByID := make(map[string]int)
	for i, spec := range specs {
		refs.docs[i] = documentOf(i, spec)
		if localID := refs.docs[i].localID; localID != "" {
			indexByLocal[localID] = i
		}
		if id := refs.docs[i].id; id != "" {
			indexByID[id] = i
		}
	}
	for i, doc := range refs.docs {
		j, inFile := indexByLocal[doc.parent]
		if !inFile {
			j, inFile = indexByID[doc.parent]
		}
		if inFile {
			refs.parent[i] = j
		} else if doc.parent != "" {
			refs.existingParent[i] = doc.parent
		}
		for _, dep := range doc.blockedBy {
			j, inFile := indexByLocal[dep]
			if !inFile {
				j, inFile = indexByID[dep]
			}
			if inFile {
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
func judgeReferences(format string, refs fileReferences, existingTypes map[string]model.IssueType) ([]int, error) {
	if err := rejectBlocksWithinFileEpics(format, refs, existingTypes); err != nil {
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
			return model.ValidationError{Message: fmt.Sprintf("%s: cycle detected involving %q", format, refs.docs[i].ref())}
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

// rejectBlocksWithinFileEpics holds the file's own blocked_by edges to the
// rule every blocked-by write meets, over the hierarchy the file describes,
// so a document blocked by an epic it sits under in the file, an epic blocked
// by a document under it, or a leaf blocked by another leaf of its epic is
// refused before the first create rather than at its edge, after the last.
// A parent is placed only if it is an epic, as the engines' own trees place
// it: a create the file names as a parent is one by now — the validators
// refused any other — and an update document or an existing parent is one by
// the type it is stored with. An existing parent is keyed apart from the
// documents, whatever its id spells. An edge whose judgment needs a real id's
// own place is the write's to judge. [LAW:single-enforcer]
func rejectBlocksWithinFileEpics(format string, refs fileReferences, existingTypes map[string]model.IssueType) error {
	node := strconv.Itoa
	tree := make(EpicTree, len(refs.parent)+len(refs.existingParent))
	for i, p := range refs.parent {
		if refs.docs[p].issueType.IsContainer() {
			tree.Place(node(i), refs.docs[i].issueType, node(p))
		}
	}
	for i, id := range refs.existingParent {
		if existingTypes[id].IsContainer() {
			tree.Place(node(i), refs.docs[i].issueType, "existing "+id)
		}
	}
	for i, deps := range refs.blockedBy {
		for _, j := range deps {
			if err := RejectBlocksWithinEpic(tree, node(i), node(j)); err != nil {
				return fmt.Errorf("%s: %s blocked_by %q: %w", format, refs.docs[i].name, refs.docs[j].ref(), err)
			}
		}
	}
	return nil
}

// parseImportTreeSpecs judges a tree-import file by every rule but those
// over the graph its references draw, which judgeReferences makes, and
// returns each document parsed, by position.
func parseImportTreeSpecs(specs []ImportTreeSpec) (map[int]FileCreate, error) {
	if len(specs) == 0 {
		return nil, model.ValidationError{Message: "import: no issues in input"}
	}
	types := make(map[string]model.IssueType, len(specs))
	creates := make(map[int]FileCreate, len(specs))
	for i, spec := range specs {
		if strings.TrimSpace(spec.LocalID) == "" {
			return nil, model.ValidationError{Message: fmt.Sprintf("import: spec %d missing local_id", i)}
		}
		if spec.LocalID != strings.TrimSpace(spec.LocalID) {
			return nil, model.ValidationError{Message: fmt.Sprintf("import: spec %d local_id %q has surrounding whitespace", i, spec.LocalID)}
		}
		if strings.TrimSpace(spec.Title) == "" {
			return nil, model.ValidationError{Message: fmt.Sprintf("import: spec %q missing title", spec.LocalID)}
		}
		// Topic, labels, and assignee parse by the parsers the create runs,
		// so a file the create would refuse is refused here, before anything
		// lands. [LAW:single-enforcer]
		if _, err := issueid.NormalizeTopicForCreate(spec.Topic); err != nil {
			return nil, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		if _, err := model.CanonicalizeLabels(spec.Labels); err != nil {
			return nil, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		if _, err := model.NormalizeAssignee(spec.Assignee); err != nil {
			return nil, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		// Membership comes from the one ParseIssueType gate; specs are
		// agent-authored input, so they parse strictly here rather than
		// relying on the salvage convention. The parsed type and priority are
		// what the create writes, and the containment checks below judge by
		// the type. [LAW:parse-dont-validate]
		issueType, err := model.ParseIssueType(spec.IssueType)
		if err != nil {
			return nil, fmt.Errorf("import: spec %q: %w", spec.LocalID, invalidValue("type", spec.IssueType, err))
		}
		priority, err := model.ParsePriority(spec.Priority)
		if err != nil {
			return nil, fmt.Errorf("import: spec %q: %w", spec.LocalID, invalidValue("priority", spec.Priority, err))
		}
		if _, dup := types[spec.LocalID]; dup {
			return nil, model.ValidationError{Message: fmt.Sprintf("import: duplicate local_id %q", spec.LocalID)}
		}
		types[spec.LocalID] = issueType
		creates[i] = FileCreate{Index: i, LocalID: spec.LocalID, Parent: spec.Parent, BlockedBy: spec.BlockedBy, issue: CreateIssueInput{
			Title:       spec.Title,
			Description: spec.Description,
			Prompt:      spec.Prompt,
			Plan:        spec.Plan,
			IssueType:   issueType,
			Topic:       spec.Topic,
			Priority:    priority,
			Assignee:    spec.Assignee,
			Labels:      spec.Labels,
		}}
	}
	for _, spec := range specs {
		if spec.Parent != "" {
			if spec.Parent != strings.TrimSpace(spec.Parent) {
				return nil, model.ValidationError{Message: fmt.Sprintf("import: spec %q parent %q has surrounding whitespace", spec.LocalID, spec.Parent)}
			}
			parentType, ok := types[spec.Parent]
			if !ok {
				return nil, model.ValidationError{Message: fmt.Sprintf("import: spec %q references missing parent %q", spec.LocalID, spec.Parent)}
			}
			if err := RejectParentThatIsNotAContainer(spec.Parent, parentType); err != nil {
				return nil, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
			}
		}
		if err := RejectLeafWithoutParent(types[spec.LocalID], spec.Parent); err != nil {
			return nil, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		if err := rejectRepeatedBlockedBy(spec.BlockedBy); err != nil {
			return nil, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		for _, dep := range spec.BlockedBy {
			if dep != strings.TrimSpace(dep) {
				return nil, model.ValidationError{Message: fmt.Sprintf("import: spec %q blocked_by entry %q has surrounding whitespace", spec.LocalID, dep)}
			}
			if _, ok := types[dep]; !ok {
				return nil, model.ValidationError{Message: fmt.Sprintf("import: spec %q references missing blocked_by %q", spec.LocalID, dep)}
			}
			if dep == spec.LocalID {
				return nil, model.ValidationError{Message: fmt.Sprintf("import: spec %q cannot depend on itself", spec.LocalID)}
			}
		}
	}
	return creates, nil
}

// parseBulkSpecs judges a bulk file by every rule but those over the
// graph its references draw, which judgeReferences makes:
// every document is a create or an update by whether it names an id, and each
// shape admits exactly the fields its verb owns. A document's parent may be a
// local_id in the file or a real id; only the first can be judged here. It
// returns each create parsed, by position, and the rest of the file parsed in
// file order: each update document's parent, blocked_by set and patch, and the
// file's ordering, every document stating `rank: top`.
func parseBulkSpecs(specs []BulkIssueSpec) (BulkFile, map[int]FileCreate, error) {
	if len(specs) == 0 {
		return BulkFile{}, nil, model.ValidationError{Message: "bulk: no issues in input"}
	}
	seenID := make(map[string]struct{}, len(specs))
	localTypes := make(map[string]model.IssueType, len(specs))
	creates := make(map[int]FileCreate, len(specs))
	var file BulkFile
	for i, spec := range specs {
		if spec.ID != strings.TrimSpace(spec.ID) {
			return BulkFile{}, nil, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d id %q has surrounding whitespace", i, spec.ID)}
		}
		if spec.LocalID != strings.TrimSpace(spec.LocalID) {
			return BulkFile{}, nil, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d local_id %q has surrounding whitespace", i, spec.LocalID)}
		}
		if parent := valueOr(spec.Parent, ""); parent != strings.TrimSpace(parent) {
			return BulkFile{}, nil, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d parent %q has surrounding whitespace", i, parent)}
		}
		// A create and an update state blocked_by by the same rules, and a
		// document names itself by its id on an update, its local_id on a
		// create. [LAW:single-enforcer]
		blockedBy := valueOr(spec.BlockedBy, nil)
		for _, dep := range blockedBy {
			if dep != strings.TrimSpace(dep) {
				return BulkFile{}, nil, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d blocked_by entry %q has surrounding whitespace", i, dep)}
			}
			if self := cmp.Or(spec.ID, spec.LocalID); self != "" && dep == self {
				return BulkFile{}, nil, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (%q) cannot depend on itself", i, self)}
			}
		}
		if err := rejectRepeatedBlockedBy(blockedBy); err != nil {
			return BulkFile{}, nil, fmt.Errorf("bulk: doc %d: %w", i, err)
		}
		// A create and an update both write labels and the assignee, by the
		// one parser either write runs. [LAW:single-enforcer]
		if spec.Labels != nil {
			if _, err := model.CanonicalizeLabels(*spec.Labels); err != nil {
				return BulkFile{}, nil, fmt.Errorf("bulk: doc %d: %w", i, err)
			}
		}
		if spec.Assignee != nil {
			if _, err := model.NormalizeAssignee(*spec.Assignee); err != nil {
				return BulkFile{}, nil, fmt.Errorf("bulk: doc %d: %w", i, err)
			}
		}

		ranks, err := parseBulkRank(spec.Rank)
		if err != nil {
			return BulkFile{}, nil, fmt.Errorf("bulk: doc %d: %w", i, err)
		}
		if ranks {
			file.Ranked = append(file.Ranked, FileRef{id: spec.ID, index: i})
		}

		if spec.ID != "" {
			patch, err := parseBulkUpdateDoc(i, spec)
			if err != nil {
				return BulkFile{}, nil, err
			}
			if _, dup := seenID[spec.ID]; dup {
				return BulkFile{}, nil, model.ValidationError{Message: fmt.Sprintf("bulk: duplicate id %q", spec.ID)}
			}
			seenID[spec.ID] = struct{}{}
			if spec.Parent != nil {
				file.Reparents = append(file.Reparents, FileReparent{ID: spec.ID, Parent: *spec.Parent})
			}
			if spec.BlockedBy != nil {
				file.BlockedBy = append(file.BlockedBy, FileBlockedBy{ID: spec.ID, BlockedBy: *spec.BlockedBy})
			}
			// The document states a field exactly when the patch it becomes
			// does, so the question is asked of that patch; a document that
			// only reparents, rewires or ranks its issue writes no field
			// change, so it has no change event for a reason to annotate.
			// [LAW:one-source-of-truth] [LAW:no-silent-failure]
			if patch.IsEmpty() {
				if !ranks && spec.Parent == nil && spec.BlockedBy == nil {
					return BulkFile{}, nil, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) has no fields to update, no parent, no blocked_by and no rank", i, spec.ID)}
				}
				if patch.Reason != "" {
					return BulkFile{}, nil, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) sets reason but changes no field; reason annotates a field change, and parent, blocked_by and rank write none", i, spec.ID)}
				}
				continue
			}
			file.Updates = append(file.Updates, BulkUpdate{ID: spec.ID, Patch: patch})
			continue
		}

		create, err := parseBulkCreateDoc(i, spec)
		if err != nil {
			return BulkFile{}, nil, err
		}
		creates[i] = create
		if spec.LocalID != "" {
			if _, dup := localTypes[spec.LocalID]; dup {
				return BulkFile{}, nil, model.ValidationError{Message: fmt.Sprintf("bulk: duplicate local_id %q", spec.LocalID)}
			}
			localTypes[spec.LocalID] = create.issue.IssueType
		}
	}
	for i, spec := range specs {
		parent := valueOr(spec.Parent, "")
		if create, isCreate := creates[i]; isCreate {
			if err := RejectLeafWithoutParent(create.issue.IssueType, parent); err != nil {
				return BulkFile{}, nil, fmt.Errorf("bulk: doc %d: %w", i, err)
			}
		}
		if parentType, inFile := localTypes[parent]; inFile {
			if err := RejectParentThatIsNotAContainer(parent, parentType); err != nil {
				return BulkFile{}, nil, fmt.Errorf("bulk: doc %d: %w", i, err)
			}
		}
	}
	return file, creates, nil
}

// parseBulkCreateDoc admits a create document and returns the create it
// writes. A field the document leaves out is the create's zero value, and an
// unstated priority is normal.
func parseBulkCreateDoc(i int, spec BulkIssueSpec) (FileCreate, error) {
	if spec.Title == nil || strings.TrimSpace(*spec.Title) == "" {
		return FileCreate{}, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d missing title", i)}
	}
	if spec.Topic == nil {
		return FileCreate{}, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d missing topic", i)}
	}
	if _, err := issueid.NormalizeTopicForCreate(*spec.Topic); err != nil {
		return FileCreate{}, fmt.Errorf("bulk: doc %d: %w", i, err)
	}
	if spec.IssueType == nil {
		return FileCreate{}, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d missing type", i)}
	}
	issueType, err := model.ParseIssueType(*spec.IssueType)
	if err != nil {
		return FileCreate{}, fmt.Errorf("bulk: doc %d: %w", i, invalidValue("type", *spec.IssueType, err))
	}
	priority := model.PriorityNormal
	if spec.Priority != nil {
		if priority, err = model.ParsePriority(*spec.Priority); err != nil {
			return FileCreate{}, fmt.Errorf("bulk: doc %d: %w", i, invalidValue("priority", *spec.Priority, err))
		}
	}
	if spec.Reason != "" {
		return FileCreate{}, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d sets reason without id (reason only applies to updates)", i)}
	}
	return FileCreate{Index: i, LocalID: spec.LocalID, Parent: valueOr(spec.Parent, ""), BlockedBy: valueOr(spec.BlockedBy, nil), issue: CreateIssueInput{
		Title:       strings.TrimSpace(*spec.Title),
		Description: valueOr(spec.Description, ""),
		Prompt:      valueOr(spec.Prompt, ""),
		Plan:        valueOr(spec.Plan, ""),
		IssueType:   issueType,
		Topic:       strings.TrimSpace(*spec.Topic),
		Priority:    priority,
		Assignee:    valueOr(spec.Assignee, ""),
		Lane:        valueOr(spec.Lane, ""),
		Labels:      valueOr(spec.Labels, nil),
		// Placement stays at its zero value, RankBottom, as a tree import's
		// does: both are `lit import --path <file>`, so a batch keeps its
		// file order in the ranked order whichever format the file is, with
		// no flag to remember. [LAW:one-source-of-truth]
	}}, nil
}

func valueOr[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
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

// parseBulkRank reads a document's rank: whether it joins the file's
// ordering. The ordering is one RankSet, which stacks the issues it names at
// the top of their frame, so "top" is the one placement it can write.
func parseBulkRank(rank *string) (bool, error) {
	if rank == nil {
		return false, nil
	}
	if *rank != "top" {
		return false, model.ValidationError{Message: fmt.Sprintf("rank %q is not a placement; the one placement is top, which stacks the file's rank: top documents at the top of their frame in file order", *rank)}
	}
	return true, nil
}

// parseBulkUpdateDoc refuses the fields an update document has no business
// setting, and otherwise returns the patch it applies, which is empty for a
// document that only reparents, rewires or ranks its issue. Each refused field
// is one no update makes — topic is immutable, a local_id names a create — so
// a document that states one is asking for something this path would silently
// not do.
func parseBulkUpdateDoc(i int, spec BulkIssueSpec) (UpdateIssueInput, error) {
	if spec.LocalID != "" {
		return UpdateIssueInput{}, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) sets local_id; local_id only applies to new tickets", i, spec.ID)}
	}
	if spec.Topic != nil {
		return UpdateIssueInput{}, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) sets topic; topic is immutable and update cannot change it", i, spec.ID)}
	}
	if spec.Title != nil && strings.TrimSpace(*spec.Title) == "" {
		return UpdateIssueInput{}, model.ValidationError{Message: fmt.Sprintf("bulk: doc %d (id %q) sets an empty title", i, spec.ID)}
	}
	patch, err := bulkUpdatePatch(spec)
	if err != nil {
		return UpdateIssueInput{}, fmt.Errorf("bulk: doc %d (id %q): %w", i, spec.ID, err)
	}
	return patch, nil
}
