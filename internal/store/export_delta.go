package store

import (
	"context"
	"database/sql"
	"reflect"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The provenance replay lands one commit per folded commit, and a folded commit
// almost always touches a handful of rows out of the whole backlog. Rewriting
// every table for every step spends O(chain × backlog) DB writes to express
// O(chain × touched) of change. This file is the diff that closes that gap: the
// transition from one export to the next as the rows that actually differ.
//
// [FRAMING:representation] the export is the map, the live tables are the
// territory; a delta is only meaningful relative to a KNOWN territory, so
// nothing here takes "the previous export" from a caller's belief — spineWriter
// owns that state and seeds it from a real read.

// The row deletes this file addresses, and the primary-key types that address
// them, live in row_deletes.go — under both this layer and the CRUD commands
// that share them. [LAW:one-way-deps]

// tableDelta is the change to one table: the keys whose current row must go,
// and the rows that must land. A row whose value changed under a stable key
// appears in BOTH — removed by key, then re-added — so no UPDATE statement (and
// no second copy of the table's column list) has to exist.
// [LAW:one-type-per-behavior] five tables, one cutter; only the key function and
// the two SQL statements differ, and those cross the boundary as values.
type tableDelta[K comparable, R any] struct {
	remove []K
	add    []R
}

// empty reports whether the delta asks for no work at all — the common case for
// a table a folded commit never touched.
func (d tableDelta[K, R]) empty() bool {
	return len(d.remove) == 0 && len(d.add) == 0
}

// diffTable computes the transition from the rows currently live to the rows
// wanted, comparing each row's PERSISTED projection so a change in a non-key
// column is caught, and keying deletions so the row can be addressed in SQL.
//
// It walks the input SLICES rather than the index maps so the emitted order is
// the caller's order rather than Go's randomized map order — a deterministic
// delta is a reviewable one, and identical inputs must produce an identical
// statement sequence. [LAW:no-ambient-temporal-coupling]
//
// Two decisions carry the comparison, and they pull in opposite directions.
//
// `persisted` narrows WHAT is compared to what the table stores. For most
// tables the model type is the row and the projection is the identity
// (wholeRow); for issues it is issueRowValues, because model.Issue is a
// hydrated view that also carries labels from another table and, for a
// container, a lifecycle derived from its children. Comparing those made every
// label edit and every child status change look like a row change and dragged
// the issue's whole cascade through a needless rewrite.
//
// reflect.DeepEqual then makes the comparison TOTAL over whatever the
// projection yields, so a column added to the projection is compared without
// anyone remembering to extend a hand-written field list. Its failure direction
// is the safe one — two values that are semantically equal but structurally
// distinct (say, equal instants in different time.Location) are reported as
// changed, which costs a redundant rewrite of that one row and never a missed
// one. [LAW:types-are-the-program]
//
// Indexing by key collapses same-key rows, which would hide a duplicate the
// database should have rejected — but only for rows reached through `live`,
// and `live` is always an export this code just produced: merge.ThreeWay keys
// every child table by exactly these primary keys and emits from a map, so a
// projected export cannot carry two rows for one key. A restore's untrusted
// export arrives as `wanted` against an empty `live`, where the add loop walks
// the slice and every duplicate still reaches the INSERT and still fails
// loudly. [LAW:no-silent-failure]
func diffTable[K comparable, R any](live, wanted []R, key func(R) K, persisted func(R) any) tableDelta[K, R] {
	liveByKey := make(map[K]R, len(live))
	for _, row := range live {
		liveByKey[key(row)] = row
	}
	wantedByKey := make(map[K]R, len(wanted))
	for _, row := range wanted {
		wantedByKey[key(row)] = row
	}
	var delta tableDelta[K, R]
	for _, row := range live {
		k := key(row)
		if want, ok := wantedByKey[k]; !ok || !reflect.DeepEqual(persisted(row), persisted(want)) {
			delta.remove = append(delta.remove, k)
		}
	}
	for _, row := range wanted {
		if have, ok := liveByKey[key(row)]; !ok || !reflect.DeepEqual(persisted(have), persisted(row)) {
			delta.add = append(delta.add, row)
		}
	}
	return delta
}

// wholeRow is the projection for the tables whose model type IS their row —
// relations, comments, labels, and events all carry exactly their persisted
// columns (an event's Changes included, since those are its child rows). Only
// issues needs a narrower projection, because only model.Issue is a hydrated
// view carrying fields no issues column holds.
func wholeRow[R any](row R) any { return row }

// exportDelta is the whole transition, one tableDelta per table, in the order
// the foreign keys demand: issues exist before anything references them.
type exportDelta struct {
	issues    tableDelta[string, model.Issue]
	relations tableDelta[relationKey, model.Relation]
	comments  tableDelta[string, model.Comment]
	labels    tableDelta[labelKey, model.Label]
	events    tableDelta[string, model.IssueEvent]
}

// empty reports whether the two exports hold the same rows, so a caller can tell
// a genuinely no-op step from one that merely looks small.
func (d exportDelta) empty() bool {
	return d.issues.empty() && d.relations.empty() && d.comments.empty() && d.labels.empty() && d.events.empty()
}

// diffExports derives the row work that turns live tables holding prev into
// tables holding next.
//
// The whole correctness of this function rests on one schema fact: every child
// table is ON DELETE CASCADE from issues (and issue_event_changes from
// issue_events). So a child row's survival is NOT what prev says — it is what
// prev says MINUS everything whose parent issue this delta is about to delete.
// cascadeSurvivors names that set, and each child diff is computed against the
// post-cascade live rows rather than against prev's rows. Get this wrong in
// either direction and the tables silently disagree with the export, which in
// the replay means a corrupted commit diff with nothing to point at.
//
// Nested event changes need no layer of their own: IssueEvent carries its
// Changes, so an event whose changes differ is a changed VALUE — removed and
// re-added — and its rows cascade with it.
//
// [LAW:effects-at-boundaries] pure: the SQL lives in applyExportDelta.
func diffExports(prev, next model.Export) exportDelta {
	survivors := cascadeSurvivors(prev.Issues, next.Issues)
	return exportDelta{
		issues: diffTable(prev.Issues, next.Issues,
			func(i model.Issue) string { return i.ID },
			func(i model.Issue) any { return issueRowValues(i) }),
		relations: diffTable(
			// A relation cascades away if EITHER endpoint does.
			filterRows(prev.Relations, func(r model.Relation) bool { return survivors[r.SrcID] && survivors[r.DstID] }),
			next.Relations,
			func(r model.Relation) relationKey {
				return relationKey{srcID: r.SrcID, dstID: r.DstID, kind: r.Type}
			}, wholeRow),
		comments: diffTable(
			filterRows(prev.Comments, func(c model.Comment) bool { return survivors[c.IssueID] }),
			next.Comments,
			func(c model.Comment) string { return c.ID }, wholeRow),
		labels: diffTable(
			filterRows(prev.Labels, func(l model.Label) bool { return survivors[l.IssueID] }),
			next.Labels,
			func(l model.Label) labelKey { return labelKey{issueID: l.IssueID, name: l.Name} }, wholeRow),
		events: diffTable(
			filterRows(prev.Events, func(e model.IssueEvent) bool { return survivors[e.IssueID] }),
			next.Events,
			func(e model.IssueEvent) string { return e.ID }, wholeRow),
	}
}

// cascadeSurvivors names the issue ids whose row this delta will NOT delete —
// present in both exports with an identical persisted row. Every other id is
// either gone from next or changed, and both cases are expressed as a DELETE
// (which takes the issue's children with it) followed by an INSERT, so that no
// UPDATE statement restating the issues column list has to exist and drift.
//
// It must decide survival by exactly the rule the issues diff uses, or the two
// disagree and the cascade accounting is wrong in one direction or the other:
// an issue the diff deletes but this calls a survivor leaves its children
// unrestored, and an issue this calls dead but the diff keeps loses them
// outright. Both share issueRowValues for that reason — the persisted row, not
// the hydrated model.Issue, whose Labels and container lifecycle belong to
// other tables and other rows entirely. [LAW:single-enforcer]
func cascadeSurvivors(prev, next []model.Issue) map[string]bool {
	nextByID := make(map[string]model.Issue, len(next))
	for _, issue := range next {
		nextByID[issue.ID] = issue
	}
	survivors := make(map[string]bool, len(prev))
	for _, issue := range prev {
		if want, ok := nextByID[issue.ID]; ok && reflect.DeepEqual(issueRowValues(issue), issueRowValues(want)) {
			survivors[issue.ID] = true
		}
	}
	return survivors
}

func filterRows[R any](rows []R, keep func(R) bool) []R {
	out := make([]R, 0, len(rows))
	for _, row := range rows {
		if keep(row) {
			out = append(out, row)
		}
	}
	return out
}

// applyExportDelta performs the delta inside one transaction, deleting before
// inserting in every table so a row whose value changed under a stable key can
// be re-added without colliding with itself, and touching issues first so that
// every child row inserted after it has its foreign key satisfied.
func applyExportDelta(ctx context.Context, tx *sql.Tx, delta exportDelta) error {
	if err := applyTableDelta(ctx, tx, delta.issues, deleteIssueTx, insertIssueTx); err != nil {
		return err
	}
	if err := applyTableDelta(ctx, tx, delta.relations, deleteRelationRowTx, insertRelationTx); err != nil {
		return err
	}
	if err := applyTableDelta(ctx, tx, delta.comments, deleteCommentTx, insertCommentTx); err != nil {
		return err
	}
	if err := applyTableDelta(ctx, tx, delta.labels, deleteLabelTx, insertLabelTx); err != nil {
		return err
	}
	return applyTableDelta(ctx, tx, delta.events, deleteEventTx, insertEventTx)
}

// applyTableDelta is the one body every table's change runs through; the table's
// two statements cross the boundary as function values rather than selecting a
// branch. [LAW:dataflow-not-control-flow] [LAW:single-enforcer]
func applyTableDelta[K comparable, R any](
	ctx context.Context,
	tx *sql.Tx,
	delta tableDelta[K, R],
	remove func(context.Context, *sql.Tx, K) (int64, error),
	add func(context.Context, *sql.Tx, R) error,
) error {
	for _, key := range delta.remove {
		// The row count is the CRUD callers' concern: the diff already
		// established this key is present, so a zero here would be a bug the
		// next step's constraint or the equivalence test surfaces, not a
		// condition to branch on. [LAW:dataflow-not-control-flow]
		if _, err := remove(ctx, tx, key); err != nil {
			return err
		}
	}
	for _, row := range delta.add {
		if err := add(ctx, tx, row); err != nil {
			return err
		}
	}
	return nil
}
