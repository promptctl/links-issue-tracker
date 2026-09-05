package memory

import (
	"context"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// exportVersion is the export schema this engine writes. It is the contract's
// current version, not the engine's: an export is the one full-state read two
// engines both serve, so a differing version number would make two identical
// stores compare unequal.
const exportVersion = 2

// Export serializes the whole store as one value.
//
// It is the campaign's differential oracle surface, which decides two things
// about it. It carries the WHOLE store — archived and deleted work included —
// because an export that honored the listing default would silently drop
// exactly the rows a diff exists to notice. And every collection comes back in
// a total order, so two stores holding the same facts serialize to the same
// bytes rather than to the same multiset.
func (e *Engine) Export(ctx context.Context) (model.Export, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	issues, err := e.listIssues(storage.ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true})
	if err != nil {
		return model.Export{}, err
	}
	labels := make([]model.Label, 0)
	for _, rows := range e.labels {
		labels = append(labels, rows...)
	}
	slices.SortFunc(labels, func(a, b model.Label) int {
		return cmpThen(strings.Compare(a.IssueID, b.IssueID), strings.Compare(a.Name, b.Name))
	})
	return model.Export{
		Version:     exportVersion,
		WorkspaceID: e.workspaceID,
		ExportedAt:  e.clock.Now(),
		Issues:      issues,
		Relations:   slices.Clone(e.relations),
		Comments:    slices.Clone(e.comments),
		Labels:      labels,
		// sortEvents, not ListAllEvents: that method is this same expression
		// wrapped in the lock Export is already holding, and calling it here
		// deadlocks on a non-reentrant mutex. Routing through the one sort
		// helper is what keeps the ordering rule single-copy — an export whose
		// events differed from a listing's would be a divergence inside one
		// engine, before the oracle ever compared two. [LAW:single-enforcer]
		Events: sortEvents(cloneEvents(e.events)),
	}, nil
}

// cmpThen is lexicographic composition of two comparisons: the second decides
// only what the first left tied.
func cmpThen(first, second int) int {
	if first != 0 {
		return first
	}
	return second
}
