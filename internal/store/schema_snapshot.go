package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// applicationTables lists the tables our goose migrations own. The schema
// snapshot covers exactly these — goose_db_version is excluded so the
// snapshot does not couple to goose's internal table shape (which can move
// between goose versions).
//
// Order matters: the snapshot is rendered in this order so byte-for-byte
// comparisons stay stable. New tables added by future migrations get
// appended here in registration order.
var applicationTables = []string{
	"meta",
	"issues",
	"relations",
	"comments",
	"labels",
	"issue_history",
}

// schemaSnapshotHeader is the human-facing header that prefixes the canonical
// schema_snapshot.sql file. The dump function emits this header verbatim so
// the regenerated file always carries the regeneration command and the
// hand-edit warning.
const schemaSnapshotHeader = `-- AUTO-GENERATED: do not hand-edit.
-- Regenerate with: LIT_REGEN_SCHEMA_SNAPSHOT=1 go test ./internal/store -run TestSchemaSnapshotMatches
-- This file is the canonical converged schema after all registered goose migrations apply.
-- CI fails if a migration body changes the resulting schema and this file is not updated in the same commit.

`

// dumpSchemaSnapshot renders the SHOW CREATE TABLE output for every
// application table in a stable, normalized form. The returned string
// includes the schemaSnapshotHeader so it round-trips against the
// schema_snapshot.sql file byte-for-byte.
//
// [LAW:one-source-of-truth] applicationTables is the only writer of "what
// tables the snapshot covers"; the snapshot file is derived from running
// migrations against a fresh workspace and dumping these tables.
func dumpSchemaSnapshot(ctx context.Context, db *sql.DB) (string, error) {
	var b strings.Builder
	b.WriteString(schemaSnapshotHeader)
	for _, table := range applicationTables {
		ddl, err := showCreateTable(ctx, db, table)
		if err != nil {
			return "", err
		}
		b.WriteString(normalizeCreateTable(ddl))
		b.WriteString(";\n\n")
	}
	return b.String(), nil
}

// showCreateTable reads SHOW CREATE TABLE for one table. Dolt returns the
// table name in column 1 and the DDL in column 2; we keep only the DDL.
func showCreateTable(ctx context.Context, db *sql.DB, table string) (string, error) {
	var name, ddl string
	if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE "+table).Scan(&name, &ddl); err != nil {
		return "", fmt.Errorf("show create table %s: %w", table, err)
	}
	return ddl, nil
}

// normalizeCreateTable strips trailing whitespace per line and any
// statement-terminating semicolon Dolt may have appended. Line-by-line
// normalization is enough because Dolt's SHOW CREATE TABLE output is
// otherwise deterministic for a given schema state. If that ever stops
// being true, deeper canonicalization (key ordering, quoting) goes here.
func normalizeCreateTable(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	out := strings.TrimRight(strings.Join(lines, "\n"), "\n;")
	return out
}
