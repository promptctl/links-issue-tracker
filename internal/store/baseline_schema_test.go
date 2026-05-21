package store

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestBaselineSchemaParsesEmbeddedMigration pins the baseline parser against
// the actual embedded 00001_baseline.sql: every table and its full column set
// must be recovered, and no table-level constraint clause may leak in as a
// pseudo-column. This is the adoption oracle — if the parse drifts from the
// file, adoption verifies the wrong shape, so the contract is locked here.
func TestBaselineSchemaParsesEmbeddedMigration(t *testing.T) {
	schema, err := baselineSchema()
	if err != nil {
		t.Fatalf("baselineSchema() error = %v", err)
	}

	want := map[string][]string{
		"meta":                {"meta_key", "meta_value"},
		"issues":              {"id", "title", "description", "agent_prompt", "status", "priority", "issue_type", "topic", "assignee", "created_at", "updated_at", "closed_at", "archived_at", "deleted_at", "item_rank"},
		"relations":           {"src_id", "dst_id", "type", "created_at", "created_by"},
		"comments":            {"id", "issue_id", "body", "created_at", "created_by"},
		"labels":              {"issue_id", "label", "created_at", "created_by"},
		"issue_events":        {"id", "issue_id", "action", "reason", "actor", "created_at"},
		"issue_event_changes": {"event_id", "field", "from_value", "to_value"},
	}

	if len(schema) != len(want) {
		t.Fatalf("parsed %d tables, want %d: got keys %v", len(schema), len(want), sortedKeys(schema))
	}
	for table, wantCols := range want {
		gotCols, ok := schema[table]
		if !ok {
			t.Errorf("missing table %q in parsed schema", table)
			continue
		}
		got := append([]string(nil), gotCols...)
		exp := append([]string(nil), wantCols...)
		sort.Strings(got)
		sort.Strings(exp)
		if strings.Join(got, ",") != strings.Join(exp, ",") {
			t.Errorf("table %q columns = %v, want %v", table, got, exp)
		}
	}
}

// TestOpenRefusesPreConvergedColumnShape pins the column-level adoption gate: a
// workspace carrying every baseline table but with a pre-converged column shape
// (here, issues missing the topic column that the baseline requires)
// must be refused, not silently stamped at v1. Table presence alone is not
// "at baseline" — this is the deeper PR #119 failure shape.
func TestOpenRefusesPreConvergedColumnShape(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// Make the workspace pre-converged: drop a column the baseline requires,
	// then remove goose history so the next Open hits the adoption gate.
	if err := first.ExecRawForTest(ctx, `ALTER TABLE issues DROP COLUMN topic`); err != nil {
		_ = first.Close()
		t.Fatalf("drop topic column error = %v", err)
	}
	if err := first.ExecRawForTest(ctx, `DROP TABLE goose_db_version`); err != nil {
		_ = first.Close()
		t.Fatalf("drop goose history error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "simulate pre-converged workspace"); err != nil {
		_ = first.Close()
		t.Fatalf("commit error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	_, err = Open(ctx, doltRoot, "test-workspace-id")
	if err == nil {
		t.Fatal("Open() on a pre-converged workspace returned nil error; expected refusal")
	}
	if !strings.Contains(err.Error(), "partial schema") {
		t.Fatalf("error %q does not explain the partial-schema refusal", err)
	}
	if !strings.Contains(err.Error(), "issues.topic") {
		t.Fatalf("error %q does not name the missing issues.topic column", err)
	}
}
