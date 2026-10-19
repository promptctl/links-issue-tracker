package store

import (
	"context"
	"database/sql"
	"testing"
)

// observeDolt opens the database at doltRoot read-only and below the migration
// gate — the connection DumpRaw reads through — and hands it to fn. It is how a
// test reads a store's version-control state (HEAD, commit log, branches) while
// the store is closed or while Open refuses it, in process rather than through
// an external dolt binary.
func observeDolt(t *testing.T, ctx context.Context, doltRoot, workspaceID string, fn func(db *sql.DB)) {
	t.Helper()
	s, err := openStoreConnection(ctx, doltRoot, workspaceID, engineRead)
	if err != nil {
		t.Fatalf("open %s read-only to observe it: %v", doltRoot, err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatalf("close observer of %s: %v", doltRoot, err)
		}
	}()
	fn(s.db)
}

// observedHead is the HEAD commit of the closed store at doltRoot.
func observedHead(t *testing.T, ctx context.Context, doltRoot, workspaceID string) string {
	t.Helper()
	var head string
	observeDolt(t, ctx, doltRoot, workspaceID, func(db *sql.DB) {
		var err error
		if head, err = readDoltHead(ctx, db); err != nil {
			t.Fatal(err)
		}
	})
	return head
}

// observedBranch is the branch the closed store at doltRoot has checked out.
func observedBranch(t *testing.T, ctx context.Context, doltRoot, workspaceID string) string {
	t.Helper()
	var branch string
	observeDolt(t, ctx, doltRoot, workspaceID, func(db *sql.DB) {
		var err error
		if branch, err = activeBranch(ctx, db); err != nil {
			t.Fatal(err)
		}
	})
	return branch
}

// commitMessages is every commit message on db's current branch, newest first.
func commitMessages(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	return queryStrings(t, ctx, db, `SELECT message FROM dolt_log()`)
}

// branchNames is every branch in db.
func branchNames(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	return queryStrings(t, ctx, db, `SELECT name FROM dolt_branches`)
}

func queryStrings(t *testing.T, ctx context.Context, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("%s: scan: %v", query, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}
