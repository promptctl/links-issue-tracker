package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/doltcli"
)

// TestFreshOpenStampsBaselineVersion pins the fresh-workspace acceptance: Open
// applies 00001_baseline.sql, goose records v1, and the apply lands as one
// Dolt commit whose message names the migration.
func TestFreshOpenStampsBaselineVersion(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	version, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() error = %v", err)
	}
	if version != baselineVersion {
		t.Fatalf("recorded version = %d, want %d", version, baselineVersion)
	}

	log, err := doltcli.Run(ctx, filepath.Join(doltRoot, "links"), "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log error = %v", err)
	}
	if !strings.Contains(log, "migrate: v1 00001_baseline.sql") {
		t.Fatalf("dolt log missing per-migration commit message:\n%s", log)
	}
}

// TestPreGooseAdoptionStampsWithoutRerunningBaseline pins the adoption path: a
// workspace already at the canonical shape but lacking goose history is
// re-stamped at the baseline version (not re-created), and the baseline tables
// survive untouched.
func TestPreGooseAdoptionStampsWithoutRerunningBaseline(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// Seed a row so we can prove adoption preserves data (does not re-run baseline).
	if err := first.ExecRawForTest(ctx,
		`INSERT INTO issues(id, title, description, status, priority, issue_type, topic, assignee, item_rank, created_at, updated_at)
		 VALUES ('keep-me','Keep','', 'open', 0, 'task', 'misc', '', 'M', '2026-01-01', '2026-01-01')`,
	); err != nil {
		t.Fatalf("seed row error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "seed row"); err != nil {
		t.Fatalf("commit seed error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	withGooseHistoryDropped(t, ctx, doltRoot)

	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second) adoption error = %v", err)
	}
	defer second.Close()

	version, err := second.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() error = %v", err)
	}
	if version != baselineVersion {
		t.Fatalf("post-adoption version = %d, want %d", version, baselineVersion)
	}
	var seeded string
	if err := second.db.QueryRowContext(ctx, `SELECT title FROM issues WHERE id = 'keep-me'`).Scan(&seeded); err != nil {
		t.Fatalf("seeded row missing after adoption (baseline was wrongly re-run?): %v", err)
	}
	if seeded != "Keep" {
		t.Fatalf("seeded row title = %q, want %q", seeded, "Keep")
	}
}

// TestAdoptionDeletesLegacySchemaVersionKey pins the one-source-of-truth
// cleanup: after adoption, the legacy meta.schema_version key is removed so
// goose_db_version is the sole applied-state authority.
func TestAdoptionDeletesLegacySchemaVersionKey(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.ExecRawForTest(ctx, `INSERT INTO meta(meta_key, meta_value) VALUES ('schema_version', '1')`); err != nil {
		t.Fatalf("seed legacy schema_version error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "seed legacy schema_version"); err != nil {
		t.Fatalf("commit legacy key error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	withGooseHistoryDropped(t, ctx, doltRoot)

	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second) adoption error = %v", err)
	}
	defer second.Close()

	var present int
	err = second.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE meta_key = 'schema_version'`).Scan(&present)
	if err != nil {
		t.Fatalf("query legacy key error = %v", err)
	}
	if present != 0 {
		t.Fatal("adoption did not delete legacy meta.schema_version key")
	}
}

// withStore opens the workspace, runs body on the live Store, and closes
// unconditionally — so the recovery test bodies that use it read as
// straight-line SQL with no teardown ladder. [LAW:dataflow-not-control-flow]
// cleanup is one defer that always fires, not an `if err != nil { _ = Close() }`
// site at every call. Other tests in this file (TestFreshOpenStampsBaselineVersion,
// TestPreGooseAdoption*) predate this helper and still call Open directly; they
// can migrate opportunistically. The deferred Close() asserts its own error so
// driver/shutdown failures don't get swallowed silently.
func withStore(t *testing.T, ctx context.Context, doltRoot string, body func(*Store)) {
	t.Helper()
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	body(st)
}

// mustExec runs a raw SQL exec, failing the test on error. It exists so that
// test bodies read as a sequence of SQL statements, not a tower of `if err`.
func mustExec(t *testing.T, ctx context.Context, st *Store, q string, args ...any) {
	t.Helper()
	if err := st.ExecRawForTest(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// mustCommit commits the working set, failing the test on error.
func mustCommit(t *testing.T, ctx context.Context, st *Store, msg string) {
	t.Helper()
	if err := st.commitWorkingSet(ctx, msg); err != nil {
		t.Fatalf("commit %q: %v", msg, err)
	}
}

// stampGooseVersionAhead records an applied goose version one past the registry
// max, simulating a workspace written by a newer binary. It commits the working
// set so a subsequent Open observes the stamp from the committed working set.
func stampGooseVersionAhead(t *testing.T, ctx context.Context, doltRoot string) int64 {
	t.Helper()
	registryMax, err := registryMaxVersion()
	if err != nil {
		t.Fatalf("registryMaxVersion() error = %v", err)
	}
	ahead := registryMax + 1
	withStore(t, ctx, doltRoot, func(st *Store) {
		mustExec(t, ctx, st, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, ahead)
		mustCommit(t, ctx, st, "stamp ahead version for test")
	})
	return ahead
}

// TestOpenReconcilesAheadOfRegistryWhenBaselineIntact pins the recovery path
// for the May 23 incident shape: a workspace whose goose_db_version is ahead
// of this binary's registry but whose live application tables are intact
// MUST auto-reconcile (trim the bookkeeping log down to the registry max)
// instead of refusing. Application data is never touched; only the goose
// rows above registryMax are removed.
//
// [LAW:types-are-the-program] The refusal type's MissingBaseline field is
// the discriminator: this path returns no error because the live schema is
// intact, so MissingBaseline would be empty. A test for the other branch
// (corrupt baseline) lives below.
func TestOpenReconcilesAheadOfRegistryWhenBaselineIntact(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	ahead := stampGooseVersionAhead(t, ctx, doltRoot)
	registryMax := ahead - 1

	withStore(t, ctx, doltRoot, func(st *Store) {
		recorded, err := st.recordedMigrationVersion(ctx)
		if err != nil {
			t.Fatalf("recordedMigrationVersion() error = %v", err)
		}
		if recorded != registryMax {
			t.Errorf("post-reconcile recorded version = %d, want %d (trimmed to registry max)", recorded, registryMax)
		}

		var aheadCount int
		if err := st.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM goose_db_version WHERE version_id > ?`, registryMax,
		).Scan(&aheadCount); err != nil {
			t.Fatalf("query post-reconcile goose rows error = %v", err)
		}
		if aheadCount != 0 {
			t.Errorf("goose_db_version still has %d rows above registry max %d", aheadCount, registryMax)
		}
	})
}

// TestOpenReconcilesAheadOfRegistryWhenGooseHistoryCorrupt pins the post-DELETE
// invariant: even if goose_db_version is corrupted (rows at or below registryMax
// are missing, leaving only the ahead row), reconciliation must leave
// recordedMigrationVersion equal to registryMaxVers — not 0 or any other value
// < registryMaxVers. Without the restamp, the next Open would see applied=0
// and try to re-baseline against an already-initialized schema.
//
// [LAW:types-are-the-program] The post-reconcile workspace state must satisfy
// the invariant "managed at registryMaxVers." This test asserts that invariant
// holds even when the only goose row at recovery time is the ahead row.
func TestOpenReconcilesAheadOfRegistryWhenGooseHistoryCorrupt(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	ahead := stampGooseVersionAhead(t, ctx, doltRoot)
	registryMax := ahead - 1

	// stampGooseVersionAhead inserted the ahead row but did not reconcile (no
	// subsequent Open ran). The withStore below opens the workspace, which
	// invokes recovery and trims the ahead row before the body runs. Inside
	// the body, delete every row <= registryMax and re-insert the ahead row,
	// so the next Open sees only the ahead row — the corruption shape the
	// post-DELETE restamp invariant is meant to handle.
	withStore(t, ctx, doltRoot, func(st *Store) {
		mustExec(t, ctx, st, `DELETE FROM goose_db_version WHERE version_id <= ?`, registryMax)
		mustExec(t, ctx, st, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, ahead)
		mustCommit(t, ctx, st, "test: corrupt goose history")
	})

	withStore(t, ctx, doltRoot, func(st *Store) {
		recorded, err := st.recordedMigrationVersion(ctx)
		if err != nil {
			t.Fatalf("recordedMigrationVersion() error = %v", err)
		}
		if recorded != registryMax {
			t.Errorf("post-reconcile recorded version = %d, want %d (restamped after empty DELETE)", recorded, registryMax)
		}
	})
}

// TestOpenRefusesAheadOfRegistryWhenBaselineCorrupt pins the refusal branch:
// when goose is ahead AND the live baseline shape is genuinely missing
// (e.g. a baseline column was dropped), Open MUST refuse with the
// MissingBaseline field populated so the operator can see what the binary
// cannot operate against. This is the only path that still surfaces
// UnsupportedSchemaVersionError.
func TestOpenRefusesAheadOfRegistryWhenBaselineCorrupt(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	withStore(t, ctx, doltRoot, func(st *Store) {
		mustExec(t, ctx, st, `ALTER TABLE issues DROP COLUMN title`)
		mustCommit(t, ctx, st, "test: corrupt baseline shape")
	})

	ahead := stampGooseVersionAhead(t, ctx, doltRoot)
	registryMax := ahead - 1

	_, err := Open(ctx, doltRoot, "test-workspace-id")
	if err == nil {
		t.Fatal("Open() of corrupt-baseline ahead-of-registry workspace returned nil; want refusal")
	}
	var unsupported *UnsupportedSchemaVersionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Open() error = %v (%T); want *UnsupportedSchemaVersionError", err, err)
	}
	if unsupported.WorkspaceVersion != ahead {
		t.Errorf("WorkspaceVersion = %d, want %d", unsupported.WorkspaceVersion, ahead)
	}
	if unsupported.MaxSupported != registryMax {
		t.Errorf("MaxSupported = %d, want %d", unsupported.MaxSupported, registryMax)
	}
	if len(unsupported.MissingBaseline) == 0 {
		t.Error("MissingBaseline empty; want at least one entry naming the dropped column")
	}
	found := false
	for _, m := range unsupported.MissingBaseline {
		if strings.Contains(m, "issues.title") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("MissingBaseline = %v, want an entry naming issues.title", unsupported.MissingBaseline)
	}
}

// TestUnsupportedSchemaVersionMessageShape pins the operator-facing remediation
// string so it cannot silently regress: it must name both versions and use the
// forward-only "please upgrade lit" phrasing, never "delete" or "manual SQL".
// When MissingBaseline is populated, the message additionally surfaces the
// schema gaps so the operator can diagnose the genuine-incompatibility branch.
func TestUnsupportedSchemaVersionMessageShape(t *testing.T) {
	msg := (&UnsupportedSchemaVersionError{WorkspaceVersion: 7, MaxSupported: 3}).Error()

	want := "please upgrade lit (your workspace is at schema version 7; this binary supports up to 3)"
	if msg != want {
		t.Fatalf("Error() = %q, want %q", msg, want)
	}
	for _, forbidden := range []string{"delete", "DELETE", "manual SQL", "drop", "DROP"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("remediation message contains forbidden phrase %q: %q", forbidden, msg)
		}
	}

	withGaps := (&UnsupportedSchemaVersionError{
		WorkspaceVersion: 7,
		MaxSupported:     3,
		MissingBaseline:  []string{"issues.title", "comments"},
	}).Error()
	if !strings.Contains(withGaps, "please upgrade lit") {
		t.Errorf("MissingBaseline message lost the forward-only remediation phrasing: %q", withGaps)
	}
	if !strings.Contains(withGaps, "issues.title") || !strings.Contains(withGaps, "comments") {
		t.Errorf("MissingBaseline message did not name the schema gaps: %q", withGaps)
	}
}

// TestOpenAllowsWorkspaceExactlyAtMax pins the boundary on the allow side: a
// workspace stamped exactly at the registry max opens cleanly as a no-op (the
// guard refuses strictly-greater only).
func TestOpenAllowsWorkspaceExactlyAtMax(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(fresh) error = %v", err)
	}
	registryMax, err := registryMaxVersion()
	if err != nil {
		t.Fatalf("registryMaxVersion() error = %v", err)
	}
	atMax, err := first.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() error = %v", err)
	}
	if atMax != registryMax {
		t.Fatalf("fresh workspace stamped at %d, want registry max %d", atMax, registryMax)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() of workspace at exactly the max version = %v; want clean open", err)
	}
	defer second.Close()
}
