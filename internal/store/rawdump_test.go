package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// findTable returns the dumped table with the given name, failing the test if
// the dump omitted it — the dump is supposed to be total over every table.
func findTable(t *testing.T, dump RawDump, name string) RawTable {
	t.Helper()
	for _, table := range dump.Tables {
		if table.Name == name {
			return table
		}
	}
	t.Fatalf("dump has no %q table; tables present: %v", name, tableNames(dump))
	return RawTable{}
}

func tableNames(dump RawDump) []string {
	names := make([]string, len(dump.Tables))
	for i, table := range dump.Tables {
		names[i] = table.Name
	}
	return names
}

func columnIndex(t *testing.T, table RawTable, column string) int {
	t.Helper()
	for i, c := range table.Columns {
		if c == column {
			return i
		}
	}
	t.Fatalf("table %q has no %q column; columns: %v", table.Name, column, table.Columns)
	return -1
}

// TestDumpRawReleasesDeadendedWorkspace is the foundation acceptance: a
// workspace stamped past this binary's registry max with a corrupt baseline —
// the links-recovery-icqp deadend shape — is refused by store.Open(), yet
// DumpRaw releases its application rows without ever calling Open().
func TestDumpRawReleasesDeadendedWorkspace(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const workspaceID = "test-workspace-id"

	// Seed real application data through the normal write path, then capture
	// the IDs the dump must later surface.
	var ids []string
	withStore(t, ctx, doltRoot, func(st *Store) {
		for _, title := range []string{"first rescue subject", "second rescue subject"} {
			issue, err := st.CreateIssue(ctx, CreateIssueInput{
				Title: title, IssueType: "task", Topic: "recovery", Prefix: "links",
			})
			if err != nil {
				t.Fatalf("CreateIssue(%q) error = %v", title, err)
			}
			ids = append(ids, issue.ID)
		}
	})

	// Drop a baseline column and stamp the goose log past the registry: this is
	// the genuine-incompatibility deadend (live schema is missing shape this
	// binary requires), so Open() must refuse rather than auto-reconcile.
	withStore(t, ctx, doltRoot, func(st *Store) {
		mustExec(t, ctx, st, `ALTER TABLE issues DROP COLUMN title`)
		mustCommit(t, ctx, st, "test: corrupt baseline shape")
	})
	stampGooseVersionAhead(t, ctx, doltRoot)

	if _, err := Open(ctx, doltRoot, workspaceID); err == nil {
		t.Fatal("Open() of deadended workspace returned nil; want refusal")
	} else {
		var unsupported *UnsupportedSchemaVersionError
		if !errors.As(err, &unsupported) {
			t.Fatalf("Open() error = %v (%T); want *UnsupportedSchemaVersionError", err, err)
		}
	}

	dump, err := DumpRaw(ctx, doltRoot, workspaceID)
	if err != nil {
		t.Fatalf("DumpRaw() error = %v", err)
	}
	if dump.WorkspaceID != workspaceID {
		t.Errorf("dump WorkspaceID = %q, want %q", dump.WorkspaceID, workspaceID)
	}

	issues := findTable(t, dump, "issues")
	if len(issues.Rows) != len(ids) {
		t.Fatalf("issues rows = %d, want %d", len(issues.Rows), len(ids))
	}
	idCol := columnIndex(t, issues, "id")
	got := map[string]bool{}
	for _, row := range issues.Rows {
		id, ok := row[idCol].(string)
		if !ok {
			t.Fatalf("id cell = %v (%T); want string", row[idCol], row[idCol])
		}
		got[id] = true
	}
	for _, id := range ids {
		if !got[id] {
			t.Errorf("dump missing issue id %q; got %v", id, got)
		}
	}

	// The dropped column must be absent from the dump, not faked: the dump
	// reflects the workspace's actual shape, making no schema assumptions.
	for _, c := range issues.Columns {
		if c == "title" {
			t.Errorf("dump issues columns include dropped %q: %v", c, issues.Columns)
		}
	}

	// Totality: the goose bookkeeping table is dumped like any other, and an
	// empty table's Rows is [] (non-nil), never null.
	goose := findTable(t, dump, gooseVersionTable)
	if goose.Rows == nil {
		t.Errorf("%q Rows is nil; want non-nil (possibly empty) slice", gooseVersionTable)
	}
}

// TestDumpRawHealthyWorkspaceRoundTripsValues pins value fidelity on a normal,
// openable workspace: text columns survive as strings (driver []byte
// normalized) and a NULL column scans to nil, distinct from an empty string.
func TestDumpRawHealthyWorkspaceRoundTripsValues(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const workspaceID = "test-workspace-id"

	var id string
	withStore(t, ctx, doltRoot, func(st *Store) {
		issue, err := st.CreateIssue(ctx, CreateIssueInput{
			Title: "fidelity subject", IssueType: "task", Topic: "recovery", Prefix: "links",
		})
		if err != nil {
			t.Fatalf("CreateIssue error = %v", err)
		}
		id = issue.ID
	})

	dump, err := DumpRaw(ctx, doltRoot, workspaceID)
	if err != nil {
		t.Fatalf("DumpRaw() error = %v", err)
	}

	issues := findTable(t, dump, "issues")
	if len(issues.Rows) != 1 {
		t.Fatalf("issues rows = %d, want 1", len(issues.Rows))
	}
	row := issues.Rows[0]

	titleCol := columnIndex(t, issues, "title")
	if title, ok := row[titleCol].(string); !ok || title != "fidelity subject" {
		t.Errorf("title cell = %v (%T); want string %q", row[titleCol], row[titleCol], "fidelity subject")
	}

	idCol := columnIndex(t, issues, "id")
	if got, ok := row[idCol].(string); !ok || got != id {
		t.Errorf("id cell = %v (%T); want string %q", row[idCol], row[idCol], id)
	}

	// A nullable column unset at create time (an open issue has no close time)
	// is NULL in the row, which must scan to nil — not "" — so the artifact
	// distinguishes the two.
	closedCol := columnIndex(t, issues, "closed_at")
	if row[closedCol] != nil {
		t.Errorf("closed_at cell = %v (%T); want nil for an open issue", row[closedCol], row[closedCol])
	}
}
