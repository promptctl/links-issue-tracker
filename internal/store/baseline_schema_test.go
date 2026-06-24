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

// TestOpenForwardMigratesPreConvergedColumnShape pins the contract that a
// workspace carrying every baseline table but with a pre-converged column
// shape (here, issues missing the topic column the baseline requires) is
// FORWARD-MIGRATED to v1 — not refused. This is the recovery from the
// commit-254f86b deletion that stranded such workspaces in "partial schema,
// restore or recreate" (destroy your data) refusals.
//
// [LAW:no-silent-failure] Old workspaces at any prior canonical shape
// reach v1 by forward migration, not by being told to recreate themselves.
// [LAW:dataflow-not-control-flow] The reconcile is idempotent and probe-
// driven; the missing column gets filled regardless of which earlier shape
// the workspace was at.
func TestOpenForwardMigratesPreConvergedColumnShape(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// Seed a real issue so the forward migration must preserve it.
	if _, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Pre-migration issue", Topic: "fwd", IssueType: "task", Priority: 0}); err != nil {
		_ = first.Close()
		t.Fatalf("seed CreateIssue error = %v", err)
	}
	// Simulate a pre-converged shape: drop a column the baseline requires
	// and remove goose history so the next Open hits the adoption path.
	if err := first.ExecRawForTest(ctx, `ALTER TABLE issues DROP COLUMN topic`); err != nil {
		_ = first.Close()
		t.Fatalf("drop topic column error = %v", err)
	}
	// Revert post-baseline migrations and strip goose history so the next Open
	// hits the adoption path on a genuine pre-goose (baseline-shaped) workspace.
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	// Reopen — reconcile must forward-migrate the workspace, NOT refuse it.
	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second) error = %v — reconcile must forward-migrate pre-converged workspaces, not refuse", err)
	}
	defer second.Close()
	// topic column is back at the canonical shape; verifyBaselineShape says yes.
	_, missing, err := second.verifyBaselineShape(ctx)
	if err != nil {
		t.Fatalf("verifyBaselineShape() error = %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("after forward migration, baseline shape is still missing: %v", missing)
	}
	// Adoption stamps baseline, then Open forward-migrates to HEAD.
	v, err := second.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() error = %v", err)
	}
	if v != headVersion(t) {
		t.Fatalf("goose version = %d, want %d", v, headVersion(t))
	}
	// The seeded row survived the forward migration with its data intact.
	issues, err := second.ListIssues(ctx, ListIssuesFilter{SearchTerms: []string{"Pre-migration issue"}})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue to survive forward migration, got %d", len(issues))
	}
	if issues[0].Title != "Pre-migration issue" {
		t.Fatalf("issue title corrupted by migration: got %q", issues[0].Title)
	}
}
