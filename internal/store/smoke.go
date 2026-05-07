package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// smokeProbe is one SELECT the doctor runs against a critical table. Each
// probe lists the columns the binary actually depends on so a missing column
// fails the probe rather than silently returning NULL elsewhere.
type smokeProbe struct {
	Name string
	SQL  string
}

// smokeProbes is the canonical smoke-test set. Goal: "if any fail, the
// schema is materially wrong and downstream operations will misbehave," not
// "every column is exhaustively verified." The snapshot canary already
// covers exhaustive structure.
//
// [LAW:one-source-of-truth] applicationTables and smokeProbes share the
// same coverage commitment — every application table in the snapshot has
// one probe here. New table → add to both.
var smokeProbes = []smokeProbe{
	{Name: "meta", SQL: "SELECT meta_key, meta_value FROM meta LIMIT 1"},
	{Name: "issues", SQL: "SELECT id, title, status, priority, issue_type, topic, item_rank FROM issues LIMIT 1"},
	{Name: "relations", SQL: "SELECT src_id, dst_id, type FROM relations LIMIT 1"},
	{Name: "comments", SQL: "SELECT id, issue_id, body FROM comments LIMIT 1"},
	{Name: "labels", SQL: "SELECT issue_id, label FROM labels LIMIT 1"},
	{Name: "issue_history", SQL: "SELECT id, issue_id, action, from_status, to_status FROM issue_history LIMIT 1"},
	{Name: "migration_quarantine", SQL: "SELECT version_id, reason, quarantined_at FROM migration_quarantine LIMIT 1"},
}

// runSmokeTests executes every probe in registration order. Returns the
// first failing probe's name plus its error. Empty name means all probes
// passed. [LAW:dataflow-not-control-flow] every probe always runs the same
// query+close pair; only the SELECT text varies.
func (s *Store) runSmokeTests(ctx context.Context) (string, error) {
	for _, p := range smokeProbes {
		rows, err := s.db.QueryContext(ctx, p.SQL)
		if err != nil {
			return p.Name, err
		}
		rows.Close()
	}
	return "", nil
}

// readLastAppliedMigration returns the highest applied version (skipping
// goose's seed version 0) and its tstamp string, or zero values if no real
// migration is recorded. Used by the smoke-test failure message so the
// agent sees which migration the breakage likely came from.
func readLastAppliedMigration(ctx context.Context, db *sql.DB) (int64, string, error) {
	exists, err := tableExists(ctx, db, gooseVersionTable)
	if err != nil {
		return 0, "", err
	}
	if !exists {
		return 0, "", nil
	}
	var version int64
	var ts sql.NullString
	err = db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT version_id, tstamp FROM %s
			WHERE is_applied = TRUE AND version_id > 0
			ORDER BY id DESC LIMIT 1`, gooseVersionTable)).Scan(&version, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("read last applied migration: %w", err)
	}
	if !ts.Valid {
		return version, "", nil
	}
	return version, ts.String, nil
}
