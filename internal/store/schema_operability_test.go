package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// declareMinWriterSchema runs the statement migrations/README.md gives a
// migration that changes a write rule, with value as the version it records.
func declareMinWriterSchema(t *testing.T, ctx context.Context, st *Store, value string) {
	t.Helper()
	mustExec(t, ctx, st,
		`INSERT INTO meta (meta_key, meta_value) VALUES ('min_writer_schema', ?) ON DUPLICATE KEY UPDATE meta_value = ?`,
		value, value)
}

// stampWriteRuleChangeAhead leaves the workspace as a newer lit's migration one
// past this binary's registry leaves it when that migration declares minWriter
// as the store's minimum writer schema ("" declares none). It returns the
// version the workspace is then at.
func stampWriteRuleChangeAhead(t *testing.T, ctx context.Context, doltRoot, minWriter string) int64 {
	t.Helper()
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("migrations.MaxVersion() error = %v", err)
	}
	ahead := registryMax + 1
	withStore(t, ctx, doltRoot, func(st *Store) {
		mustExec(t, ctx, st, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, ahead)
		if minWriter != "" {
			declareMinWriterSchema(t, ctx, st, minWriter)
		}
		mustCommit(t, ctx, st, "test: a newer lit's migration")
	})
	return ahead
}

// TestWriteOpenRefusesAWorkspaceWhoseWriteRulesItPredates pins the contract:
// a lit older than a migration that changed a write rule refuses to write to
// the migrated workspace, naming the schema version to install a lit for, and
// still reads it.
func TestWriteOpenRefusesAWorkspaceWhoseWriteRulesItPredates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("migrations.MaxVersion() error = %v", err)
	}
	ahead := stampWriteRuleChangeAhead(t, ctx, doltRoot, fmt.Sprint(registryMax+1))

	_, err = Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	var unsupported *UnsupportedSchemaVersionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Open() error = %v (%T); want *UnsupportedSchemaVersionError", err, err)
	}
	if unsupported.WorkspaceVersion != ahead || unsupported.MaxSupported != registryMax || unsupported.MinWriterSchema != ahead || len(unsupported.MissingBaseline) != 0 {
		t.Errorf("refusal = %+v, want workspace v%d, max v%d, min writer v%d, no missing baseline", unsupported, ahead, registryMax, ahead)
	}
	for _, want := range []string{
		fmt.Sprintf("schema version %d changed a write rule this binary predates, so it reads this workspace but does not write to it or sync it", ahead),
		fmt.Sprintf("install a lit that supports schema version %d:\n  lit upgrade", ahead),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message lacks %q:\n%s", want, err)
		}
	}

	reader, err := OpenForRead(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v; a lit that predates a write rule still reads", err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	assertIssuesQueryable(t, ctx, reader)
	want := storage.SchemaAhead{WorkspaceSchema: ahead, MinWriterSchema: ahead, ReadOnly: true}
	if got := reader.Migrated().SchemaAhead; got != want {
		t.Errorf("read open reported %+v, want %+v", got, want)
	}
}

// TestWriteOpenWritesAWorkspaceAheadWhoseWriteRulesItKeeps pins the other side:
// a workspace ahead of this binary whose minimum writer schema this binary
// supports, or which declares none, opens for writing, and the open reports
// the newer schema it served.
func TestWriteOpenWritesAWorkspaceAheadWhoseWriteRulesItKeeps(t *testing.T) {
	t.Parallel()
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("migrations.MaxVersion() error = %v", err)
	}
	for name, tc := range map[string]struct {
		declared string
		want     int64
	}{
		"none declared":       {"", 0},
		"one this binary has": {fmt.Sprint(registryMax), registryMax},
		"an older one it has": {"7", 7},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			doltRoot := filepath.Join(t.TempDir(), "dolt")
			ahead := stampWriteRuleChangeAhead(t, ctx, doltRoot, tc.declared)
			withStore(t, ctx, doltRoot, func(st *Store) {
				want := storage.SchemaAhead{WorkspaceSchema: ahead, MinWriterSchema: tc.want}
				if got := st.Migrated().SchemaAhead; got != want {
					t.Errorf("write open reported %+v, want %+v", got, want)
				}
				mustExec(t, ctx, st, `INSERT INTO meta (meta_key, meta_value) VALUES ('test_write', 'ok')`)
			})
		})
	}
}

// TestAMinWriterSchemaThatIsNotAVersionRefusesWritesOnly pins that a
// declaration lit cannot parse leaves the minimum unknown: the write open
// refuses with the reason named, never reading it as no declaration, and the
// read open still serves the workspace, since reading obeys no write rule.
func TestAMinWriterSchemaThatIsNotAVersionRefusesWritesOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	ahead := stampWriteRuleChangeAhead(t, ctx, doltRoot, "v12")

	want := `meta min_writer_schema = "v12" is not a schema version`
	_, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	var unsupported *UnsupportedSchemaVersionError
	if !errors.As(err, &unsupported) || unsupported.WorkspaceVersion != ahead || unsupported.MinWriterSchemaErr == nil {
		t.Fatalf("Open() error = %v (%T); want *UnsupportedSchemaVersionError at v%d with the parse error", err, err, ahead)
	}
	if !strings.Contains(err.Error(), "its minimum writer schema cannot be read ("+want) {
		t.Errorf("refusal message lacks the parse error %q:\n%s", want, err)
	}

	reader, err := OpenForRead(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v; a lit that cannot read the minimum still reads", err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	assertIssuesQueryable(t, ctx, reader)
	got := reader.Migrated().SchemaAhead
	if got.WorkspaceSchema != ahead || !got.ReadOnly || got.MinWriterSchemaErr == nil || !strings.Contains(got.MinWriterSchemaErr.Error(), want) {
		t.Errorf("read open reported %+v, want v%d read-only with the parse error", got, ahead)
	}
}

// TestSyncOpenMeetsTheWriteFloor pins that the sync engine, which writes,
// refuses a workspace whose write rules this binary predates as the migrating
// open does, and opens one ahead whose write rules it keeps, reporting the
// newer schema it served.
func TestSyncOpenMeetsTheWriteFloor(t *testing.T) {
	t.Parallel()
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("migrations.MaxVersion() error = %v", err)
	}
	for name, tc := range map[string]struct {
		declared string
		refused  bool
	}{
		"a write rule it predates": {fmt.Sprint(registryMax + 1), true},
		"one it keeps":             {fmt.Sprint(registryMax), false},
		"one it cannot read":       {"v12", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			doltRoot := filepath.Join(t.TempDir(), "dolt")
			ahead := stampWriteRuleChangeAhead(t, ctx, doltRoot, tc.declared)

			st, err := OpenSync(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
			var unsupported *UnsupportedSchemaVersionError
			if refused := errors.As(err, &unsupported); refused != tc.refused {
				t.Fatalf("OpenSync() error = %v (%T); want refused %v", err, err, tc.refused)
			}
			if tc.refused {
				if unsupported.WorkspaceVersion != ahead {
					t.Errorf("refusal = %+v, want workspace v%d", unsupported, ahead)
				}
				// The same step the migrating open refuses at.
				if step := asConvergenceError(t, err).Step; step != stepRefuseUnwritable {
					t.Errorf("refusal step = %s, want %s", step, stepRefuseUnwritable)
				}
				return
			}
			defer func() {
				if err := st.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
				}
			}()
			want := storage.SchemaAhead{WorkspaceSchema: ahead, MinWriterSchema: registryMax}
			if got := st.Migrated().SchemaAhead; got != want {
				t.Errorf("sync open reported %+v, want %+v", got, want)
			}
		})
	}
}

// TestRollbackSnapshotIsOneTheOlderLitWrites pins the refusal and the rollback
// it offers to one rule: across real migrations, a snapshot whose minimum
// writer schema is above the lit is passed over for the next older one, since
// restoring it would leave a workspace the lit refuses to write.
func TestRollbackSnapshotIsOneTheOlderLitWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	snaps := rollbackHistory(t, ctx, doltRoot)
	fromV5, fromV3 := snaps[0], snaps[1]

	snap, err := openStoreConnection(ctx, testEnv(), testStderr(), fromV5.Path, "test-workspace-id", engineWrite)
	if err != nil {
		t.Fatalf("open snapshot %s: %v", fromV5.Path, err)
	}
	declareMinWriterSchema(t, ctx, snap, "5")
	mustCommit(t, ctx, snap, "test: v5 changed a write rule")
	if err := snap.Close(); err != nil {
		t.Fatalf("close snapshot: %v", err)
	}

	withStore(t, ctx, doltRoot, func(st *Store) {
		for maxSupported, want := range map[int64]string{
			2: fromV3.Name,
			4: fromV3.Name,
			5: fromV5.Name,
		} {
			if got := st.searchRollbackSnapshot(ctx, maxSupported); got.err != nil || got.snapshot != want {
				t.Errorf("search(max %d) = %q (err %v), want %q", maxSupported, got.snapshot, got.err, want)
			}
		}
	})
}
