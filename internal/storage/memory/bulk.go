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
// reported rather than inferred. The file's ordering is written after the
// creates as one RankSet, and updates are applied last, so a failing create,
// edge, or ordering leaves none applied, but a failing update does not revert
// the ones before it, which is why the error text is the caller's only
// complete account of what happened. [LAW:no-silent-failure]
func (e *Engine) BulkApply(ctx context.Context, prefix, actor string, specs []storage.BulkIssueSpec) (storage.BulkApplyResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	parentTypes := map[string]model.IssueType{}
	for _, id := range storage.BulkParentRefs(specs) {
		if parent, ok := e.issues[id]; ok {
			parentTypes[id] = parent.issueType
		}
	}
	file, err := storage.ParseBulkFile(specs, parentTypes)
	if err != nil {
		return storage.BulkApplyResult{}, err
	}

	batch := newBatch(len(specs))
	for _, create := range file.Creates {
		issue, err := e.createIssue(create.Input(batch.resolve(create.Parent), prefix, actor))
		if err != nil {
			return storage.BulkApplyResult{}, e.compensate(batch, actor, fmt.Errorf("bulk: create doc %d: %w", create.Index, err))
		}
		batch.created(create.Index, create.LocalID, issue.ID)
	}
	for _, create := range file.Creates {
		for _, dep := range create.BlockedBy {
			if err := e.wireDependency(batch.realID(create.Index), batch.resolve(dep), actor); err != nil {
				return storage.BulkApplyResult{}, e.compensate(batch, actor, fmt.Errorf("bulk: blocked_by doc %d -> %q: %w", create.Index, dep, err))
			}
		}
	}
	result := storage.BulkApplyResult{Created: batch.createdMappings()}
	if ranked := file.RankedIDs(batch.createdIDs); len(ranked) > 0 {
		if result.Ranked, err = e.rankSet(ranked); err != nil {
			return storage.BulkApplyResult{}, e.compensate(batch, actor, fmt.Errorf("bulk: rank: %w", err))
		}
	}
	for _, update := range file.Updates {
		applied, err := e.apply(update.ID, storage.Change{Actor: actor, Fields: update.Patch})
		if err != nil {
			return storage.BulkApplyResult{}, e.compensate(batch, actor, fmt.Errorf("bulk: update %q: %w; %s", update.ID, err, result.Kept()))
		}
		result.Updated = append(result.Updated, applied.Issue.ID)
	}
	return result, nil
}

// ImportTree creates a whole issue tree, returning the local-ID → real-ID
// mapping the caller needs to talk about what it just made. It compensates on
// failure exactly as BulkApply does, for the same reason.
func (e *Engine) ImportTree(ctx context.Context, prefix, actor string, specs []storage.ImportTreeSpec) (storage.ImportTreeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	creates, err := storage.ParseImportTreeFile(specs)
	if err != nil {
		return storage.ImportTreeResult{}, err
	}

	batch := newBatch(len(specs))
	for _, create := range creates {
		issue, err := e.createIssue(create.Input(batch.resolveLocal(create.Parent), prefix, actor))
		if err != nil {
			return storage.ImportTreeResult{}, e.compensate(batch, actor, fmt.Errorf("import: create %q: %w", create.LocalID, err))
		}
		batch.created(create.Index, create.LocalID, issue.ID)
	}
	for _, create := range creates {
		for _, dep := range create.BlockedBy {
			if err := e.wireDependency(batch.realID(create.Index), batch.resolveLocal(dep), actor); err != nil {
				return storage.ImportTreeResult{}, e.compensate(batch, actor, fmt.Errorf("import: blocked_by %q -> %q: %w", create.LocalID, dep, err))
			}
		}
	}
	return storage.ImportTreeResult{Created: batch.createdMappings()}, nil
}

func (e *Engine) wireDependency(dependent, dependency, actor string) error {
	_, err := e.addRelation(storage.AddRelationInput{
		SrcID: dependent, DstID: dependency, Type: model.RelBlockedBy, CreatedBy: actor,
	})
	return err
}

// compensate undoes the issues this batch created and folds what it could not
// undo into the error the caller sees. The original failure travels unchanged;
// the rollback only adds an account of what is left behind. [LAW:no-silent-failure]
func (e *Engine) compensate(batch *batchIDs, actor string, cause error) error {
	leaked := []string{}
	for _, id := range batch.createdIDs {
		if _, err := e.apply(id, storage.Change{Action: model.Delete{}, Actor: actor, Reason: "import rollback"}); err != nil {
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
