package store

import (
	"context"
	"database/sql"
	"fmt"
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

// relationKey is a relations row's primary key. A relation also carries
// created_at/created_by OUTSIDE that key, so two relations can share a key and
// still differ — which is why every diff below compares whole VALUES and only
// deletes by key. [LAW:types-are-the-program] the key type is exactly the
// schema's PRIMARY KEY (src_id, dst_id, type), so a partial key cannot be built.
type relationKey struct {
	srcID string
	dstID string
	kind  model.RelationType
}

// labelKey is a labels row's primary key (issue_id, label).
type labelKey struct {
	issueID string
	name    string
}

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
// wanted, comparing whole values so a change in a non-key column is caught, and
// keying deletions so the row can be addressed in SQL.
//
// It walks the input SLICES rather than the index maps so the emitted order is
// the caller's order rather than Go's randomized map order — a deterministic
// delta is a reviewable one, and identical inputs must produce an identical
// statement sequence. [LAW:no-ambient-temporal-coupling]
//
// reflect.DeepEqual is the comparison on purpose: it is TOTAL over the row
// struct, so a column added to the schema and the model is compared without
// anyone remembering to extend a hand-written field list. Its failure direction
// is also the safe one — two values that are semantically equal but structurally
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
func diffTable[K comparable, R any](live, wanted []R, key func(R) K) tableDelta[K, R] {
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
		if want, ok := wantedByKey[k]; !ok || !reflect.DeepEqual(row, want) {
			delta.remove = append(delta.remove, k)
		}
	}
	for _, row := range wanted {
		if have, ok := liveByKey[key(row)]; !ok || !reflect.DeepEqual(have, row) {
			delta.add = append(delta.add, row)
		}
	}
	return delta
}

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
		issues: diffTable(prev.Issues, next.Issues, func(i model.Issue) string { return i.ID }),
		relations: diffTable(
			// A relation cascades away if EITHER endpoint does.
			filterRows(prev.Relations, func(r model.Relation) bool { return survivors[r.SrcID] && survivors[r.DstID] }),
			next.Relations,
			func(r model.Relation) relationKey {
				return relationKey{srcID: r.SrcID, dstID: r.DstID, kind: r.Type}
			}),
		comments: diffTable(
			filterRows(prev.Comments, func(c model.Comment) bool { return survivors[c.IssueID] }),
			next.Comments,
			func(c model.Comment) string { return c.ID }),
		labels: diffTable(
			filterRows(prev.Labels, func(l model.Label) bool { return survivors[l.IssueID] }),
			next.Labels,
			func(l model.Label) labelKey { return labelKey{issueID: l.IssueID, name: l.Name} }),
		events: diffTable(
			filterRows(prev.Events, func(e model.IssueEvent) bool { return survivors[e.IssueID] }),
			next.Events,
			func(e model.IssueEvent) string { return e.ID }),
	}
}

// cascadeSurvivors names the issue ids whose row this delta will NOT delete —
// present in both exports with an identical row. Every other id is either gone
// from next or changed, and both cases are expressed as a DELETE (which takes
// the issue's children with it) followed by an INSERT, so that no UPDATE
// statement restating the issues column list has to exist and drift.
// [LAW:one-source-of-truth] the column list lives once, in the INSERT.
func cascadeSurvivors(prev, next []model.Issue) map[string]bool {
	nextByID := make(map[string]model.Issue, len(next))
	for _, issue := range next {
		nextByID[issue.ID] = issue
	}
	survivors := make(map[string]bool, len(prev))
	for _, issue := range prev {
		if want, ok := nextByID[issue.ID]; ok && reflect.DeepEqual(issue, want) {
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
	remove func(context.Context, *sql.Tx, K) error,
	add func(context.Context, *sql.Tx, R) error,
) error {
	for _, key := range delta.remove {
		if err := remove(ctx, tx, key); err != nil {
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

// Deleting an issue takes its relations, comments, labels, events and event
// changes with it — the schema's ON DELETE CASCADE — which is exactly what
// cascadeSurvivors accounts for.
func deleteIssueTx(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM issues WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete issue %s: %w", id, err)
	}
	return nil
}

func deleteRelationRowTx(ctx context.Context, tx *sql.Tx, key relationKey) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND dst_id = ? AND type = ?`,
		key.srcID, key.dstID, key.kind); err != nil {
		return fmt.Errorf("delete relation %s->%s (%s): %w", key.srcID, key.dstID, key.kind, err)
	}
	return nil
}

func deleteCommentTx(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM comments WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete comment %s: %w", id, err)
	}
	return nil
}

func deleteLabelTx(ctx context.Context, tx *sql.Tx, key labelKey) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE issue_id = ? AND label = ?`, key.issueID, key.name); err != nil {
		return fmt.Errorf("delete label %s:%s: %w", key.issueID, key.name, err)
	}
	return nil
}

// Deleting an event takes its issue_event_changes rows with it.
func deleteEventTx(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM issue_events WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete issue event %s: %w", id, err)
	}
	return nil
}
