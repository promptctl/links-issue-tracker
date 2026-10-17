package memory

import (
	"context"
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

	parentTypes := map[string]model.IssueType{}
	for _, id := range storage.BulkParentRefs(specs) {
		if parent, ok := e.issues[id]; ok {
			parentTypes[id] = parent.issueType
		}
	}
	order, err := storage.BulkWriteOrder(specs, parentTypes)
	if err != nil {
		return storage.BulkApplyResult{}, err
	}

	batch := newBatch(len(specs))
	result := storage.BulkApplyResult{}
	for _, index := range order {
		spec := specs[index]
		if spec.ID != "" {
			change, err := bulkUpdateChange(spec, actor)
			if err != nil {
				return storage.BulkApplyResult{}, err
			}
			applied, err := e.apply(spec.ID, change)
			if err != nil {
				return storage.BulkApplyResult{}, e.compensate(batch, fmt.Errorf("bulk: update %q: %w", spec.ID, err))
			}
			result.Updated = append(result.Updated, applied.Issue.ID)
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
	}
	result.Created = batch.createdMappings()
	for index, spec := range specs {
		for _, dep := range spec.BlockedBy {
			if err := e.wireDependency(batch.realID(index), batch.resolve(dep)); err != nil {
				return storage.BulkApplyResult{}, e.compensate(batch, fmt.Errorf("bulk: blocked_by doc %d -> %q: %w", index, dep, err))
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

	order, err := storage.ImportTreeWriteOrder(specs)
	if err != nil {
		return storage.ImportTreeResult{}, err
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
		for _, dep := range spec.BlockedBy {
			if err := e.wireDependency(batch.realID(index), batch.resolveLocal(dep)); err != nil {
				return storage.ImportTreeResult{}, e.compensate(batch, fmt.Errorf("import: blocked_by %q -> %q: %w", spec.LocalID, dep, err))
			}
		}
	}
	return storage.ImportTreeResult{Created: batch.createdMappings()}, nil
}

func (e *Engine) wireDependency(dependent, dependency string) error {
	_, err := e.addRelation(storage.AddRelationInput{
		SrcID: dependent, DstID: dependency, Type: model.RelBlockedBy, CreatedBy: createdBy,
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
// holds one mapping, so a parent reference, a blocked_by reference, and the
// caller's report all read the same resolution rather than three that could
// disagree. [LAW:one-source-of-truth]
type batchIDs struct {
	byIndex   []string
	byLocalID map[string]string
	// createdIDs is the compensation order's input: the ids this batch made,
	// oldest first.
	createdIDs []string
	// mappings is the caller's report: every create as (ref, real id), in
	// creation order, which is what keeps the rendered report identical run
	// to run.
	mappings []storage.IDMapping
}

func newBatch(size int) *batchIDs {
	return &batchIDs{byIndex: make([]string, size), byLocalID: map[string]string{}, createdIDs: make([]string, 0, size)}
}

func (b *batchIDs) created(index int, localID, realID string) {
	b.byIndex[index] = realID
	b.createdIDs = append(b.createdIDs, realID)
	b.mappings = append(b.mappings, storage.NewIDMapping(localID, realID))
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

func (b *batchIDs) createdMappings() []storage.IDMapping { return b.mappings }

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
