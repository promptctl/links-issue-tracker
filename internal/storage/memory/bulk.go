package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// BulkApply applies a mixed create/update batch, resolving the batch's own
// references before anything is written.
//
// Failure is compensated, not transactional, and the contract is about what
// the caller is told: a batch that fails partway undoes the issues it created
// and names every one it could not undo, so a partial application is always
// reported rather than inferred. Updates already applied are not reverted,
// which is why the error text is the caller's only complete account of what
// happened. [LAW:no-silent-failure]
func (e *Engine) BulkApply(ctx context.Context, prefix, actor string, specs []storage.BulkIssueSpec) (storage.BulkApplyResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := validateBulkSpecs(specs); err != nil {
		return storage.BulkApplyResult{}, err
	}
	order, err := creationOrder(bulkGraph(specs))
	if err != nil {
		return storage.BulkApplyResult{}, fmt.Errorf("bulk: %w", err)
	}

	batch := newBatch(len(specs))
	result := storage.BulkApplyResult{Created: map[string]string{}}
	for _, index := range order {
		spec := specs[index]
		if spec.ID != "" {
			change, err := bulkUpdateChange(spec, actor)
			if err != nil {
				return storage.BulkApplyResult{}, err
			}
			issue, err := e.apply(spec.ID, change)
			if err != nil {
				return storage.BulkApplyResult{}, e.compensate(batch, fmt.Errorf("bulk: update %q: %w", spec.ID, err))
			}
			result.Updated = append(result.Updated, issue.ID)
			continue
		}
		in, err := bulkCreateInput(spec, prefix, batch)
		if err != nil {
			return storage.BulkApplyResult{}, err
		}
		issue, err := e.createIssue(in)
		if err != nil {
			return storage.BulkApplyResult{}, e.compensate(batch, fmt.Errorf("bulk: create doc %d: %w", index, err))
		}
		batch.created(index, spec.LocalID, issue.ID)
		// A create is nameable by the LocalID it chose, or by its own new id
		// when the file gave it none — so every create is in the report.
		if spec.LocalID != "" {
			result.Created[spec.LocalID] = issue.ID
			continue
		}
		result.Created[issue.ID] = issue.ID
	}
	for index, spec := range specs {
		for _, dep := range spec.DependsOn {
			// depends_on reads "this issue depends on that one", and the store
			// runs a blocks edge dependent → dependency, so the document's own
			// issue is the edge's src.
			if err := e.wireDependency(batch.realID(index), batch.resolve(dep)); err != nil {
				return storage.BulkApplyResult{}, e.compensate(batch, fmt.Errorf("bulk: depends_on doc %d -> %q: %w", index, dep, err))
			}
		}
	}
	return result, nil
}

// ImportTree creates a whole issue tree, returning the local-ID → real-ID
// mapping the caller needs to talk about what it just made. It compensates on
// failure exactly as BulkApply does, for the same reason.
func (e *Engine) ImportTree(ctx context.Context, prefix string, specs []storage.ImportTreeSpec) (storage.ImportTreeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := validateImportSpecs(specs); err != nil {
		return storage.ImportTreeResult{}, err
	}
	order, err := creationOrder(importGraph(specs))
	if err != nil {
		return storage.ImportTreeResult{}, fmt.Errorf("import: %w", err)
	}

	batch := newBatch(len(specs))
	for _, index := range order {
		spec := specs[index]
		// The specs were gated before the first create; parsing again here is
		// that same gate producing the typed value, not a second definition of
		// what is valid.
		issueType, err := model.ParseIssueType(spec.IssueType)
		if err != nil {
			return storage.ImportTreeResult{}, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		priority, err := model.ParsePriority(spec.Priority)
		if err != nil {
			return storage.ImportTreeResult{}, fmt.Errorf("import: spec %q: %w", spec.LocalID, err)
		}
		issue, err := e.createIssue(storage.CreateIssueInput{
			Title:       spec.Title,
			Description: spec.Description,
			Prompt:      spec.Prompt,
			IssueType:   issueType,
			Topic:       spec.Topic,
			Priority:    priority,
			Assignee:    spec.Assignee,
			Labels:      spec.Labels,
			ParentID:    batch.resolveLocal(spec.Parent),
			Prefix:      prefix,
		})
		if err != nil {
			return storage.ImportTreeResult{}, e.compensate(batch, fmt.Errorf("import: create %q: %w", spec.LocalID, err))
		}
		batch.created(index, spec.LocalID, issue.ID)
	}
	for index, spec := range specs {
		for _, dep := range spec.DependsOn {
			if err := e.wireDependency(batch.realID(index), batch.resolveLocal(dep)); err != nil {
				return storage.ImportTreeResult{}, e.compensate(batch, fmt.Errorf("import: depends_on %q -> %q: %w", spec.LocalID, dep, err))
			}
		}
	}
	return storage.ImportTreeResult{IDMap: batch.idMap()}, nil
}

func (e *Engine) wireDependency(dependent, dependency string) error {
	_, err := e.addRelation(storage.AddRelationInput{
		SrcID: dependent, DstID: dependency, Type: model.RelBlocks, CreatedBy: createdBy,
	})
	return err
}

// compensate undoes the issues this batch created and folds what it could not
// undo into the error the caller sees. The original failure travels unchanged;
// the rollback only adds an account of what is left behind. [LAW:no-silent-failure]
func (e *Engine) compensate(batch *batchIDs, cause error) error {
	leaked := []string{}
	for _, id := range batch.createdIDs {
		if _, err := e.apply(id, storage.Change{Action: model.Delete{}, Actor: createdBy, Reason: "import rollback"}); err != nil {
			leaked = append(leaked, id)
		}
	}
	return fmt.Errorf("%w (rollback leaked %d: %s)", cause, len(leaked), strings.Join(leaked, ","))
}

// batchIDs is a batch's answer to "what did the file's names turn into". It
// holds one mapping, so a parent reference, a depends_on reference, and the
// caller's report all read the same resolution rather than three that could
// disagree. [LAW:one-source-of-truth]
type batchIDs struct {
	byIndex   []string
	byLocalID map[string]string
	// createdIDs is the compensation order's input: the ids this batch made,
	// oldest first.
	createdIDs []string
}

func newBatch(size int) *batchIDs {
	return &batchIDs{byIndex: make([]string, size), byLocalID: map[string]string{}, createdIDs: make([]string, 0, size)}
}

func (b *batchIDs) created(index int, localID, realID string) {
	b.byIndex[index] = realID
	b.createdIDs = append(b.createdIDs, realID)
	if localID != "" {
		b.byLocalID[localID] = realID
	}
}

func (b *batchIDs) realID(index int) string { return b.byIndex[index] }

// resolve maps a reference against this batch's names, passing anything it
// does not recognize through unchanged — a reference that matches nothing
// local is a real, pre-existing id, and the create or edge write that receives
// it is what decides whether it is a real one. [LAW:dataflow-not-control-flow]
// One unconditional lookup-with-fallback, not a branch on "is this local".
func (b *batchIDs) resolve(ref string) string {
	if real, ok := b.byLocalID[ref]; ok {
		return real
	}
	return ref
}

// resolveLocal maps a reference that must be local. A tree import validates
// every reference against the file before the first create, so an unresolved
// one here would be a name nothing in the batch answers to — and an empty
// result is what the create path reads as "no parent", which is why the
// validation, not this lookup, is what makes it total.
func (b *batchIDs) resolveLocal(ref string) string { return b.byLocalID[ref] }

func (b *batchIDs) idMap() map[string]string { return b.byLocalID }

// --- create inputs --------------------------------------------------------

func bulkCreateInput(spec storage.BulkIssueSpec, prefix string, batch *batchIDs) (storage.CreateIssueInput, error) {
	issueType, err := model.ParseIssueType(*spec.IssueType)
	if err != nil {
		return storage.CreateIssueInput{}, fmt.Errorf("bulk: %w", err)
	}
	priority := model.PriorityNormal
	if spec.Priority != nil {
		priority, err = model.ParsePriority(*spec.Priority)
		if err != nil {
			return storage.CreateIssueInput{}, fmt.Errorf("bulk: %w", err)
		}
	}
	return storage.CreateIssueInput{
		Title:       strings.TrimSpace(*spec.Title),
		Description: valueOr(spec.Description, ""),
		Prompt:      valueOr(spec.Prompt, ""),
		IssueType:   issueType,
		Topic:       strings.TrimSpace(*spec.Topic),
		ParentID:    batch.resolve(spec.Parent),
		Priority:    priority,
		Assignee:    valueOr(spec.Assignee, ""),
		Lane:        valueOr(spec.Lane, ""),
		Labels:      valueOr(spec.Labels, nil),
		// Placement stays at its zero value, so a batch keeps its file order
		// in the ranked order with no flag to remember, whichever of the two
		// authored formats the file is. [LAW:one-source-of-truth]
		Prefix: prefix,
	}, nil
}

// bulkUpdateChange turns an update document into the field patch it states:
// one pointer per field the document set, which is the same "nil means leave
// alone" the patch type carries everywhere else.
func bulkUpdateChange(spec storage.BulkIssueSpec, actor string) (storage.Change, error) {
	fields := storage.UpdateIssueInput{Reason: strings.TrimSpace(spec.Reason)}
	fields.Title = trimmedPointer(spec.Title)
	fields.Description = trimmedPointer(spec.Description)
	fields.Prompt = trimmedPointer(spec.Prompt)
	fields.Assignee = trimmedPointer(spec.Assignee)
	fields.Lane = trimmedPointer(spec.Lane)
	fields.Labels = spec.Labels
	if spec.IssueType != nil {
		issueType, err := model.ParseIssueType(*spec.IssueType)
		if err != nil {
			return storage.Change{}, fmt.Errorf("bulk: update %q: %w", spec.ID, err)
		}
		fields.IssueType = &issueType
	}
	if spec.Priority != nil {
		priority, err := model.ParsePriority(*spec.Priority)
		if err != nil {
			return storage.Change{}, fmt.Errorf("bulk: update %q: %w", spec.ID, err)
		}
		fields.Priority = &priority
	}
	return storage.Change{Actor: actor, Fields: fields}, nil
}

func trimmedPointer(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

func valueOr[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}

// --- creation order -------------------------------------------------------

// localGraph is a batch's internal reference structure: the name each document
// answers to, and the names it points at. Both authored formats reduce to it,
// so the ordering rule is stated once. [LAW:one-type-per-behavior]
type localGraph struct {
	localID []string
	refs    [][]string
}

func bulkGraph(specs []storage.BulkIssueSpec) localGraph {
	graph := localGraph{localID: make([]string, len(specs)), refs: make([][]string, len(specs))}
	for i, spec := range specs {
		graph.localID[i] = spec.LocalID
		graph.refs[i] = append(append([]string{}, spec.Parent), spec.DependsOn...)
	}
	return graph
}

func importGraph(specs []storage.ImportTreeSpec) localGraph {
	graph := localGraph{localID: make([]string, len(specs)), refs: make([][]string, len(specs))}
	for i, spec := range specs {
		graph.localID[i] = spec.LocalID
		graph.refs[i] = append(append([]string{}, spec.Parent), spec.DependsOn...)
	}
	return graph
}

// creationOrder orders the documents so every one lands after everything it
// names inside the batch. A reference matching no local name is not an edge —
// it resolves outside the batch, and the write that receives it decides
// whether it is real. Cycles among the internal edges are refused.
func creationOrder(graph localGraph) ([]int, error) {
	indexByLocal := map[string]int{}
	for i, id := range graph.localID {
		if id == "" {
			continue
		}
		indexByLocal[id] = i
	}
	const (
		unvisited = iota
		visiting
		done
	)
	state := make([]int, len(graph.localID))
	order := make([]int, 0, len(graph.localID))
	var visit func(int) error
	visit = func(i int) error {
		switch state[i] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("cycle detected involving %q", graph.localID[i])
		}
		state[i] = visiting
		for _, ref := range graph.refs[i] {
			j, internal := indexByLocal[ref]
			if !internal {
				continue
			}
			if err := visit(j); err != nil {
				return err
			}
		}
		state[i] = done
		order = append(order, i)
		return nil
	}
	for i := range graph.localID {
		if err := visit(i); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// --- validation -----------------------------------------------------------

// validateBulkSpecs enumerates the whole accept/reject shape of a bulk file
// before any document is applied, so a batch either wholly makes sense or
// fails before touching the store. There are exactly two legal document
// shapes — a create and an update — and the ID is what tells them apart.
// [LAW:types-are-the-program]
func validateBulkSpecs(specs []storage.BulkIssueSpec) error {
	if len(specs) == 0 {
		return errors.New("bulk: no issues in input")
	}
	seenID := map[string]struct{}{}
	seenLocal := map[string]struct{}{}
	for i, spec := range specs {
		for _, field := range []struct{ name, value string }{
			{"id", spec.ID}, {"local_id", spec.LocalID}, {"parent", spec.Parent},
		} {
			if field.value != strings.TrimSpace(field.value) {
				return fmt.Errorf("bulk: doc %d %s %q has surrounding whitespace", i, field.name, field.value)
			}
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
			if err := validateBulkUpdate(i, spec); err != nil {
				return err
			}
			if _, dup := seenID[spec.ID]; dup {
				return fmt.Errorf("bulk: duplicate id %q", spec.ID)
			}
			seenID[spec.ID] = struct{}{}
			continue
		}
		if err := validateBulkCreate(i, spec); err != nil {
			return err
		}
		if spec.LocalID == "" {
			continue
		}
		if _, dup := seenLocal[spec.LocalID]; dup {
			return fmt.Errorf("bulk: duplicate local_id %q", spec.LocalID)
		}
		seenLocal[spec.LocalID] = struct{}{}
	}
	return nil
}

func validateBulkCreate(i int, spec storage.BulkIssueSpec) error {
	if spec.Title == nil || strings.TrimSpace(*spec.Title) == "" {
		return fmt.Errorf("bulk: doc %d missing title", i)
	}
	if spec.Topic == nil || strings.TrimSpace(*spec.Topic) == "" {
		return fmt.Errorf("bulk: doc %d missing topic", i)
	}
	if spec.IssueType == nil {
		return fmt.Errorf("bulk: doc %d missing type", i)
	}
	if _, err := model.ParseIssueType(*spec.IssueType); err != nil {
		return fmt.Errorf("bulk: doc %d has invalid type %q", i, *spec.IssueType)
	}
	if spec.Priority != nil {
		if _, err := model.ParsePriority(*spec.Priority); err != nil {
			return fmt.Errorf("bulk: doc %d has invalid priority %d", i, *spec.Priority)
		}
	}
	if spec.Reason != "" {
		return fmt.Errorf("bulk: doc %d sets reason without id (reason only applies to updates)", i)
	}
	return nil
}

// validateBulkUpdate refuses the fields an update document has no business
// setting. Each one is somebody else's verb — topic is immutable, reparenting
// and dependency wiring are their own commands — so a document that states one
// is asking for something this path would silently not do.
func validateBulkUpdate(i int, spec storage.BulkIssueSpec) error {
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
	if !statesAnyUpdatableField(spec) {
		return fmt.Errorf("bulk: doc %d (id %q) has no fields to update", i, spec.ID)
	}
	return nil
}

func statesAnyUpdatableField(spec storage.BulkIssueSpec) bool {
	return spec.Title != nil || spec.Description != nil || spec.Prompt != nil ||
		spec.IssueType != nil || spec.Priority != nil || spec.Assignee != nil ||
		spec.Labels != nil || spec.Lane != nil
}

// validateImportSpecs gates a tree-import file. Every reference in it must
// resolve inside the file — a tree import states a whole tree — which is what
// lets the create loop below it read parent and dependency names without
// asking whether they are there.
func validateImportSpecs(specs []storage.ImportTreeSpec) error {
	if len(specs) == 0 {
		return errors.New("import: no issues in input")
	}
	named := map[string]struct{}{}
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
		if _, err := model.ParseIssueType(spec.IssueType); err != nil {
			return fmt.Errorf("import: spec %q has invalid type %q", spec.LocalID, spec.IssueType)
		}
		if _, err := model.ParsePriority(spec.Priority); err != nil {
			return fmt.Errorf("import: spec %q has invalid priority %d", spec.LocalID, spec.Priority)
		}
		if _, dup := named[spec.LocalID]; dup {
			return fmt.Errorf("import: duplicate local_id %q", spec.LocalID)
		}
		named[spec.LocalID] = struct{}{}
	}
	for _, spec := range specs {
		if spec.Parent != "" {
			if spec.Parent != strings.TrimSpace(spec.Parent) {
				return fmt.Errorf("import: spec %q parent %q has surrounding whitespace", spec.LocalID, spec.Parent)
			}
			if _, ok := named[spec.Parent]; !ok {
				return fmt.Errorf("import: spec %q references missing parent %q", spec.LocalID, spec.Parent)
			}
		}
		for _, dep := range spec.DependsOn {
			if dep != strings.TrimSpace(dep) {
				return fmt.Errorf("import: spec %q depends_on entry %q has surrounding whitespace", spec.LocalID, dep)
			}
			if dep == spec.LocalID {
				return fmt.Errorf("import: spec %q cannot depend on itself", spec.LocalID)
			}
			if _, ok := named[dep]; !ok {
				return fmt.Errorf("import: spec %q references missing depends_on %q", spec.LocalID, dep)
			}
		}
	}
	return nil
}
