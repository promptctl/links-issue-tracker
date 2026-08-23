package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The per-table row deletes, and the primary-key types that address them.
//
// They live apart from both of their callers on purpose. Two layers need them —
// the ordinary CRUD commands (RemoveRelation, RemoveLabel, DeleteComment) and
// the reconcile replay's delta — and a primitive shared by two layers belongs
// under both rather than inside whichever one happened to need it first.
// Holding them in export_delta.go made the CRUD commands reach UP into the
// diff layer for a statement that has nothing to do with diffing.
// [LAW:one-way-deps] everything here depends on the schema and nothing above it,
// so all four callers depend downhill.
//
// This is also why the deletes do not simply sit beside the insert*Tx helpers
// in import_export.go, which would be the tidier-looking arrangement: the two
// halves are not at the same depth. Deleting a row needs its key and nothing
// else, while inserting one needs the normalized whole-row projection
// (issueRowValues, and through it the storage layer's status and retention
// normalizations). Symmetry of naming is not a reason to give them the same
// home when their dependencies differ. [LAW:decomposition]

// relationKey is a relations row's primary key. A relation also carries
// created_at/created_by OUTSIDE that key, so two relations can share a key and
// still differ — which is why the delta compares whole VALUES and only deletes
// by key. [LAW:types-are-the-program] the key type is exactly the schema's
// PRIMARY KEY (src_id, dst_id, type), so a partial key cannot be built.
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

// Each delete below is the one place its table's BY-KEY, SINGLE-ROW delete
// lives, the same way the restore/replay whole-row INSERTs route through one
// insert*Tx each.
//
// Three of the five have two callers, and that is what made sharing them worth
// doing: relations, comments and labels are each deleted both by the replay's
// delta and by an ordinary CRUD command (RemoveRelation, DeleteComment,
// RemoveLabel). The other two have only the delta, and in both cases that is
// structural rather than an omission waiting to be filled. Ordinary issue
// deletion is the soft DeletedAt stamp, so no CRUD path hard-deletes an issue
// row at all. Event history is append-only from the CRUD side: events ARE
// inserted one at a time as things happen (store.go), but nothing outside this
// delta removes one by id — an event goes away only when its issue's row does,
// via the cascade. Looking for a CRUD caller of deleteIssueTx or deleteEventTx
// will turn up nothing, and nothing is the correct answer.
//
// That is deliberately narrower than "the only DELETE against this table," and
// the difference is a real one rather than an exception being excused. These
// address exactly one row by its full primary key. The other deletes in the
// package answer a different question — they remove a SET matched by a partial
// predicate — so they are separate statements on purpose, not strays that
// escaped consolidation: setSingleValuedEdgeTx (relations.go, every edge of a
// type from one source), ClearParent (relations.go, the parent edge by src and
// type), replaceLabelsTx (labels.go, every label on an issue), and the
// self-edge sweep in import_export.go. Consolidating those into a by-key helper
// would mean looking up the rows first in order to delete them one at a time,
// which is slower and no clearer. [LAW:single-enforcer] applies per operation,
// not per table.
//
// They return the affected row count because the CRUD callers need it to tell
// "removed" from "there was nothing there"; the delta ignores it, having
// already decided from the diff that the row is present.

// Deleting an issue takes its relations, comments, labels, events and event
// changes with it — the schema's ON DELETE CASCADE — which is exactly what
// cascadeSurvivors accounts for.
func deleteIssueTx(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	return execDelete(ctx, tx, `DELETE FROM issues WHERE id = ?`, fmt.Sprintf("issue %s", id), id)
}

func deleteRelationRowTx(ctx context.Context, tx *sql.Tx, key relationKey) (int64, error) {
	return execDelete(ctx, tx, `DELETE FROM relations WHERE src_id = ? AND dst_id = ? AND type = ?`,
		fmt.Sprintf("relation %s->%s (%s)", key.srcID, key.dstID, key.kind),
		key.srcID, key.dstID, string(key.kind))
}

func deleteCommentTx(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	return execDelete(ctx, tx, `DELETE FROM comments WHERE id = ?`, fmt.Sprintf("comment %s", id), id)
}

func deleteLabelTx(ctx context.Context, tx *sql.Tx, key labelKey) (int64, error) {
	return execDelete(ctx, tx, `DELETE FROM labels WHERE issue_id = ? AND label = ?`,
		fmt.Sprintf("label %s:%s", key.issueID, key.name), key.issueID, key.name)
}

// Deleting an event takes its issue_event_changes rows with it.
func deleteEventTx(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	return execDelete(ctx, tx, `DELETE FROM issue_events WHERE id = ?`, fmt.Sprintf("issue event %s", id), id)
}

// execDelete runs one delete and reports how many rows it removed, naming the
// target in any error so a failure says which row it was. [LAW:no-silent-failure]
func execDelete(ctx context.Context, tx *sql.Tx, stmt, subject string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("delete %s: %w", subject, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete %s: rows affected: %w", subject, err)
	}
	return affected, nil
}
