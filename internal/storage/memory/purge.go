package memory

import (
	"context"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Purge removes the issue from every structure that holds it — the record, its
// place in the rank order, its comments, labels, history, and every edge with
// it at either end — and keeps the purge record, under the one hold, so no
// reader sees the issue half gone.
func (e *Engine) Purge(ctx context.Context, in storage.PurgeInput) (storage.PurgeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	id := in.IssueID
	if _, ok := e.issues[id]; !ok {
		return storage.PurgeResult{}, storage.NotFoundError{Entity: "issue", ID: id}
	}
	if err := storage.RejectPurgeOfOccupiedContainer(id, e.detachedChildren(id)); err != nil {
		return storage.PurgeResult{}, err
	}
	var result storage.PurgeResult
	e.comments, result.Comments = removeWhere(e.comments, func(c model.Comment) bool { return c.IssueID == id })
	result.Relations = e.dropRelations(func(r model.Relation) bool { return r.SrcID == id || r.DstID == id })
	result.Events = len(e.events[id])
	delete(e.events, id)
	result.Labels = len(e.labels[id])
	delete(e.labels, id)
	e.detach(id)
	result.Purge = model.Purge{IssueID: id, IssueCreatedAt: e.issues[id].createdAt, PurgedAt: e.clock.Now(), PurgedBy: in.Actor}
	delete(e.issues, id)
	e.purges = append(e.purges, result.Purge)
	return result, nil
}

// detachedChildren is every child filed under parentID, with the parent edges
// it keeps once its edge to parentID is gone.
func (e *Engine) detachedChildren(parentID string) []storage.DetachedChild {
	var out []storage.DetachedChild
	for _, rel := range e.relations {
		if rel.Type != model.RelParentChild || rel.DstID != parentID {
			continue
		}
		out = append(out, storage.DetachedChild{ID: rel.SrcID, Type: e.issues[rel.SrcID].issueType, ParentsLeft: e.parentsLeftWithout(rel.SrcID, parentID)})
	}
	return out
}

// removeWhere drops the rows match selects and reports how many it dropped.
func removeWhere[R any](rows []R, match func(R) bool) ([]R, int) {
	before := len(rows)
	rows = slices.DeleteFunc(rows, match)
	return rows, before - len(rows)
}

// purgeList is the purge records in the order Export carries them in, which
// model.UnionPurges owns; each ticket holds one record, so the union only
// sorts. [LAW:one-source-of-truth]
func (e *Engine) purgeList() []model.Purge {
	return model.UnionPurges(e.purges)
}
