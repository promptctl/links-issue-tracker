package memory

import (
	"context"
	"slices"
	"strings"

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
	if err := storage.RejectPurgeOfOccupiedContainer(id, e.childCount(id)); err != nil {
		return storage.PurgeResult{}, err
	}
	var result storage.PurgeResult
	e.comments, result.Comments = removeWhere(e.comments, func(c model.Comment) bool { return c.IssueID == id })
	e.relations, result.Relations = removeWhere(e.relations, func(r model.Relation) bool { return r.SrcID == id || r.DstID == id })
	e.events, result.Events = removeWhere(e.events, func(ev model.IssueEvent) bool { return ev.IssueID == id })
	result.Labels = len(e.labels[id])
	delete(e.labels, id)
	e.order = slices.DeleteFunc(e.order, func(existing string) bool { return existing == id })
	delete(e.issues, id)
	result.Purge = model.Purge{IssueID: id, PurgedAt: e.clock.Now(), PurgedBy: in.Actor}
	e.purges[id] = result.Purge
	return result, nil
}

// removeWhere drops the rows match selects and reports how many it dropped.
func removeWhere[R any](rows []R, match func(R) bool) ([]R, int) {
	before := len(rows)
	rows = slices.DeleteFunc(rows, match)
	return rows, before - len(rows)
}

// purgeList is the purge records by issue id ascending, the order Export
// carries them in.
func (e *Engine) purgeList() []model.Purge {
	out := make([]model.Purge, 0, len(e.purges))
	for _, p := range e.purges {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b model.Purge) int { return strings.Compare(a.IssueID, b.IssueID) })
	return out
}
