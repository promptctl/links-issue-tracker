package memory

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// BulkApply applies a mixed create/update batch, resolving the batch's own
// references before anything is written, and writes it as one batch: a file
// refused at any write writes nothing, whatever it wrote before the refusal,
// and withBatch holds the observers' hearing of its updates until it lands. Every
// edge set the file states — the update documents' parents and blocked_by,
// and the creates' blocked_by — is written after the creates as one step,
// judged on the graph the file ends with; the file's ordering follows as one
// RankSet, and its updates come last. [LAW:single-enforcer] withBatch owns the
// atomicity.
func (e *Engine) BulkApply(ctx context.Context, prefix, actor string, specs []storage.BulkIssueSpec) (storage.BulkApplyResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	existingTypes := map[string]model.IssueType{}
	for _, id := range storage.BulkExistingRefs(specs) {
		if issue, ok := e.issues[id]; ok {
			existingTypes[id] = issue.issueType
		}
	}
	file, err := storage.ParseBulkFile(specs, existingTypes)
	if err != nil {
		return storage.BulkApplyResult{}, err
	}

	return withBatch(e, func(result *storage.BulkApplyResult) error {
		batch := newBatch(len(specs))
		for _, create := range file.Creates {
			issue, err := e.createIssue(create.Input(batch.resolve(create.Parent), prefix, actor))
			if err != nil {
				return fmt.Errorf("bulk: create doc %d: %w", create.Index, err)
			}
			batch.created(create.Index, create.LocalID, issue.ID)
		}
		result.Created = batch.createdMappings()
		edges, err := file.WriteEdges(batch.createdIDs, edgeWriter{e: e, actor: actor})
		if err != nil {
			return err
		}
		result.RecordEdges(edges)
		if ranked := file.RankedIDs(batch.createdIDs); len(ranked) > 0 {
			if result.Ranked, err = e.rankSet(ranked); err != nil {
				return fmt.Errorf("bulk: rank: %w", err)
			}
		}
		for _, update := range file.Updates {
			change := storage.Change{Actor: actor, Fields: update.Patch}
			applied, err := e.apply(update.ID, change)
			if err != nil {
				return fmt.Errorf("bulk: update %q: %w", update.ID, err)
			}
			e.notify(change, applied.Issue)
			result.RecordUpdate(applied)
		}
		return nil
	})
}

// ImportTree creates a whole issue tree as one batch, returning the local-ID →
// real-ID mapping the caller needs to talk about what it just made. A tree
// refused at any write writes nothing, as a bulk file does.
func (e *Engine) ImportTree(ctx context.Context, prefix, actor string, specs []storage.ImportTreeSpec) (storage.ImportTreeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	creates, err := storage.ParseImportTreeFile(specs)
	if err != nil {
		return storage.ImportTreeResult{}, err
	}

	return withBatch(e, func(result *storage.ImportTreeResult) error {
		batch := newBatch(len(specs))
		for _, create := range creates {
			issue, err := e.createIssue(create.Input(batch.resolveLocal(create.Parent), prefix, actor))
			if err != nil {
				return fmt.Errorf("import: create %q: %w", create.LocalID, err)
			}
			batch.created(create.Index, create.LocalID, issue.ID)
		}
		for _, create := range creates {
			for _, dep := range create.BlockedBy {
				if err := e.wireDependency(batch.realID(create.Index), batch.resolveLocal(dep), actor); err != nil {
					return fmt.Errorf("import: blocked_by %q -> %q: %w", create.LocalID, dep, err)
				}
			}
		}
		result.Created = batch.createdMappings()
		return nil
	})
}

// edgeWriter is a bulk file's relation write: every edge it adds goes through
// addRelation, the write `lit dep add` and `lit parent set` make, so an edge a
// file writes obeys the one rule its type carries. [LAW:single-enforcer]
type edgeWriter struct {
	e     *Engine
	actor string
}

func (w edgeWriter) Edges(id string, relType model.RelationType) ([]model.Relation, error) {
	if _, err := w.e.mustRecord(id); err != nil {
		return nil, err
	}
	var edges []model.Relation
	for _, rel := range w.e.relations {
		if rel.SrcID == id && rel.Type == relType {
			edges = append(edges, rel)
		}
	}
	return edges, nil
}

func (w edgeWriter) Add(id, target string, relType model.RelationType) error {
	_, err := w.e.addRelation(storage.AddRelationInput{SrcID: id, DstID: target, Type: relType, CreatedBy: w.actor})
	return err
}

func (w edgeWriter) Remove(edge model.Relation) error {
	w.e.dropRelations(func(rel model.Relation) bool {
		return rel.SrcID == edge.SrcID && rel.DstID == edge.DstID && rel.Type == edge.Type
	})
	return nil
}

func (e *Engine) wireDependency(dependent, dependency, actor string) error {
	_, err := e.addRelation(storage.AddRelationInput{
		SrcID: dependent, DstID: dependency, Type: model.RelBlockedBy, CreatedBy: actor,
	})
	return err
}

// batchIDs is a batch's answer to "what did the file's names turn into". It
// holds one mapping, so a parent reference, a blocked_by reference, and the
// caller's report all read the same resolution rather than three that could
// disagree. [LAW:one-source-of-truth]
type batchIDs struct {
	byIndex   []string
	byLocalID map[string]string
	// createdIDs is the ids this batch made, oldest first: the order the
	// file's edge and rank steps name its creates in.
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

// resolve maps a reference against this batch's names by the one rule every
// engine resolves a file's references by. [LAW:one-source-of-truth]
func (b *batchIDs) resolve(ref string) string { return storage.ResolveRef(ref, b.byLocalID) }

// resolveLocal maps a reference that must be local. A tree import validates
// every reference against the file before the first create, so an unresolved
// one here would be a name nothing in the batch answers to — and an empty
// result is what the create path reads as "no parent", which is why the
// validation, not this lookup, is what makes it total.
func (b *batchIDs) resolveLocal(ref string) string { return b.byLocalID[ref] }

func (b *batchIDs) createdMappings() []storage.IDMapping { return b.mappings }
