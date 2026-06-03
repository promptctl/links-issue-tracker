package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/doltcli"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/version"
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

	appliedVersion, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() error = %v", err)
	}
	if appliedVersion != baselineVersion {
		t.Fatalf("recorded version = %d, want %d", appliedVersion, baselineVersion)
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

	appliedVersion, err := second.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() error = %v", err)
	}
	if appliedVersion != baselineVersion {
		t.Fatalf("post-adoption version = %d, want %d", appliedVersion, baselineVersion)
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
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("migrations.MaxVersion() error = %v", err)
	}
	ahead := registryMax + 1
	withStore(t, ctx, doltRoot, func(st *Store) {
		mustExec(t, ctx, st, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, ahead)
		mustCommit(t, ctx, st, "stamp ahead version for test")
	})
	return ahead
}

// TestOpenToleratesAheadOfRegistryWhenBaselineIntact pins the contract for the
// May 23 incident shape: a workspace whose goose_db_version is ahead of this
// binary's registry but whose live application tables are intact MUST open and
// operate, NOT refuse. goose treats unknown-ahead rows as nothing-to-apply, so
// no bookkeeping reconciliation is needed — the ahead row is left intact, and
// re-opening is stable. (In the field an ahead row records migrations a newer
// binary really applied; the fixture synthesizes that row directly via
// stampGooseVersionAhead — the contract under test is "tolerate it and leave it
// alone", not whether the recorded migrations were executed here.)
//
// [LAW:behavior-not-structure] The contract is "Open succeeds and the workspace
// is operable", not "the log was surgically trimmed to registryMax". The old
// trim was an implementation detail (and an actively harmful one — it destroyed
// true migration history and left the live schema ahead of a reset log).
func TestOpenToleratesAheadOfRegistryWhenBaselineIntact(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	ahead := stampGooseVersionAhead(t, ctx, doltRoot)

	withStore(t, ctx, doltRoot, func(st *Store) {
		// The ahead row is preserved — not trimmed back to the registry max.
		recorded, err := st.recordedMigrationVersion(ctx)
		if err != nil {
			t.Fatalf("recordedMigrationVersion() error = %v", err)
		}
		if recorded != ahead {
			t.Errorf("recorded version = %d, want %d (ahead log preserved, not trimmed)", recorded, ahead)
		}
		// Operable: the live application schema answers queries.
		assertIssuesQueryable(t, ctx, st)
	})

	// Re-open is stable: tolerating the ahead log is idempotent, no brick.
	withStore(t, ctx, doltRoot, func(st *Store) {
		assertIssuesQueryable(t, ctx, st)
	})
}

// TestOpenToleratesGooseLogWithOnlyAheadRow pins that operability is decided by
// the live schema, not by the goose log's internal consistency: a log carrying
// ONLY an ahead row (its baseline row missing) still opens and operates, because
// the binary reads the schema — which is intact — rather than trusting the log.
// This is the corruption shape the old code restamped around; the read-only
// design makes it a non-event.
//
// [LAW:behavior-not-structure] Asserts the workspace opens and is queryable,
// not any particular post-recovery row count in goose_db_version.
func TestOpenToleratesGooseLogWithOnlyAheadRow(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	ahead := stampGooseVersionAhead(t, ctx, doltRoot)
	registryMax := ahead - 1

	// Strip every row at or below the registry max, leaving only the ahead row.
	withStore(t, ctx, doltRoot, func(st *Store) {
		mustExec(t, ctx, st, `DELETE FROM goose_db_version WHERE version_id <= ?`, registryMax)
		mustCommit(t, ctx, st, "test: leave only the ahead goose row")
	})

	withStore(t, ctx, doltRoot, func(st *Store) {
		assertIssuesQueryable(t, ctx, st)
	})
}

// assertIssuesQueryable proves the live application schema is operable by
// running a read against the canonical issues table. A missing/broken table
// would error here, distinguishing "opened and usable" from merely "Open
// returned nil".
func assertIssuesQueryable(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues`).Scan(&n); err != nil {
		t.Fatalf("issues table not queryable after Open: %v", err)
	}
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
	forbidden := []string{"delete", "DELETE", "manual SQL", "drop", "DROP"}
	assertForbiddenAbsent := func(t *testing.T, msg string) {
		t.Helper()
		for _, f := range forbidden {
			if strings.Contains(msg, f) {
				t.Errorf("remediation message contains forbidden phrase %q: %q", f, msg)
			}
		}
	}
	const upgradeHead = "please upgrade lit (your workspace is at schema version 7; this binary supports up to 3"

	// Bare refusal: no recovery data — only the upgrade phrase, no recovery
	// block. Pin exact equality so the bare form cannot silently grow.
	bare := (&UnsupportedSchemaVersionError{WorkspaceVersion: 7, MaxSupported: 3}).Error()
	if bare != upgradeHead+")" {
		t.Fatalf("bare Error() = %q, want %q", bare, upgradeHead+")")
	}
	assertForbiddenAbsent(t, bare)

	// MissingBaseline preserved from sxsk.5: gap names surface inside the
	// parenthetical alongside the upgrade phrase.
	withGaps := (&UnsupportedSchemaVersionError{
		WorkspaceVersion: 7,
		MaxSupported:     3,
		MissingBaseline:  []string{"issues.title", "comments"},
	}).Error()
	if !strings.HasPrefix(withGaps, upgradeHead) {
		t.Errorf("MissingBaseline message lost the forward-only remediation phrasing: %q", withGaps)
	}
	if !strings.Contains(withGaps, "issues.title") || !strings.Contains(withGaps, "comments") {
		t.Errorf("MissingBaseline message did not name the schema gaps: %q", withGaps)
	}
	assertForbiddenAbsent(t, withGaps)

	// Snapshot only: lossy-rollback line surfaces the verbatim snapshot
	// name; the proactive lit-downgrade line is suppressed because no
	// producer version is recorded.
	snapOnly := (&UnsupportedSchemaVersionError{
		WorkspaceVersion: 7,
		MaxSupported:     3,
		SnapshotName:     "1700000000-pre-migrate-1700000000",
	}).Error()
	if !strings.HasPrefix(snapOnly, upgradeHead) {
		t.Errorf("snapshot-only message lost the upgrade phrase: %q", snapOnly)
	}
	if !strings.Contains(snapOnly, "lit snapshots restore 1700000000-pre-migrate-1700000000") {
		t.Errorf("snapshot-only message did not surface the snapshot name verbatim: %q", snapOnly)
	}
	if !strings.Contains(snapOnly, "LOSSY") {
		t.Errorf("snapshot-only message did not flag the rollback as lossy: %q", snapOnly)
	}
	if strings.Contains(snapOnly, "lit downgrade --to") {
		t.Errorf("snapshot-only message included downgrade line without producer version: %q", snapOnly)
	}
	assertForbiddenAbsent(t, snapOnly)

	// Producer version only: lit-downgrade line surfaces the verbatim
	// version; the lossy-rollback path is named as unavailable so the user
	// knows reinstall is their only path.
	verOnly := (&UnsupportedSchemaVersionError{
		WorkspaceVersion:      7,
		MaxSupported:          3,
		ProducerBinaryVersion: "v0.4.2",
	}).Error()
	if !strings.Contains(verOnly, "lit downgrade --to v0.4.2") {
		t.Errorf("producer-only message did not surface the version verbatim: %q", verOnly)
	}
	if !strings.Contains(verOnly, "no pre-upgrade snapshot available") {
		t.Errorf("producer-only message did not declare the snapshot path unavailable: %q", verOnly)
	}
	assertForbiddenAbsent(t, verOnly)

	// Both populated: every recovery line surfaces.
	both := (&UnsupportedSchemaVersionError{
		WorkspaceVersion:      7,
		MaxSupported:          3,
		SnapshotName:          "1700000000-pre-migrate-1700000000",
		ProducerBinaryVersion: "v0.4.2",
	}).Error()
	if !strings.Contains(both, "lit snapshots restore 1700000000-pre-migrate-1700000000") {
		t.Errorf("both-populated message missing snapshot line: %q", both)
	}
	if !strings.Contains(both, "lit downgrade --to v0.4.2") {
		t.Errorf("both-populated message missing downgrade line: %q", both)
	}
	assertForbiddenAbsent(t, both)
}

// TestRefusalSurfacesRecoveryDataFromWorkspace pins the runtime wiring of the
// remediation hints: when a binary built from a known release version opens a
// workspace, it stamps meta.producer_binary_version on a successful migrate;
// when a later refusal fires, refuseIfBaselineMissing reads that row and the
// most recent migration-recovery snapshot, and surfaces both verbatim in the
// UnsupportedSchemaVersionError. Without this end-to-end probe the unit test
// for Error() rendering passes while the lookup layer that populates the
// fields could regress silently.
func TestRefusalSurfacesRecoveryDataFromWorkspace(t *testing.T) {
	const sentinel = "vSENTINEL-0.4.2"
	prev := version.Version
	version.Version = sentinel
	t.Cleanup(func() { version.Version = prev })

	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// Fresh open: runs the baseline migration -> takes a recovery snapshot
	// AND writes meta.producer_binary_version = sentinel.
	withStore(t, ctx, doltRoot, func(st *Store) {
		got, err := st.getMeta(ctx, nil, producerBinaryVersionMetaKey)
		if err != nil {
			t.Fatalf("getMeta(%q) error = %v", producerBinaryVersionMetaKey, err)
		}
		if got != sentinel {
			t.Errorf("meta.%s = %q, want %q (recordProducerBinaryVersion did not run on fresh migrate)",
				producerBinaryVersionMetaKey, got, sentinel)
		}
		// Corrupt the baseline so the next Open's reconcile path classifies
		// the workspace as genuinely incompatible.
		mustExec(t, ctx, st, `ALTER TABLE issues DROP COLUMN title`)
		mustCommit(t, ctx, st, "test: corrupt baseline shape")
	})

	stampGooseVersionAhead(t, ctx, doltRoot)

	_, err := Open(ctx, doltRoot, "test-workspace-id")
	if err == nil {
		t.Fatal("Open() returned nil; want UnsupportedSchemaVersionError")
	}
	var refusal *UnsupportedSchemaVersionError
	if !errors.As(err, &refusal) {
		t.Fatalf("Open() error = %v (%T); want *UnsupportedSchemaVersionError", err, err)
	}
	if refusal.ProducerBinaryVersion != sentinel {
		t.Errorf("ProducerBinaryVersion = %q, want %q", refusal.ProducerBinaryVersion, sentinel)
	}
	if refusal.SnapshotName == "" {
		t.Errorf("SnapshotName empty; want a migration-recovery snapshot name (one was taken on fresh Open)")
	}
	if !IsMigrationSnapshotName(refusal.SnapshotName) {
		t.Errorf("SnapshotName %q does not match the migration-stamped shape", refusal.SnapshotName)
	}
	msg := refusal.Error()
	if !strings.Contains(msg, "lit downgrade --to "+sentinel) {
		t.Errorf("Error() did not surface the producer version verbatim: %q", msg)
	}
	if !strings.Contains(msg, "lit snapshots restore "+refusal.SnapshotName) {
		t.Errorf("Error() did not surface the snapshot name verbatim: %q", msg)
	}
}

// TestProducerBinaryVersionUnstampedForDevBuild pins the dev-build guard: a
// binary with no link-time Version (IsDev == true) must NOT write a producer
// row, so a stray local build does not overwrite a real release stamp.
func TestProducerBinaryVersionUnstampedForDevBuild(t *testing.T) {
	prev := version.Version
	version.Version = ""
	t.Cleanup(func() { version.Version = prev })

	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	withStore(t, ctx, doltRoot, func(st *Store) {
		got, err := st.getMeta(ctx, nil, producerBinaryVersionMetaKey)
		if err != nil {
			t.Fatalf("getMeta error = %v", err)
		}
		if got != "" {
			t.Errorf("meta.%s = %q on dev build; want empty (no stamp)",
				producerBinaryVersionMetaKey, got)
		}
	})
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
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("migrations.MaxVersion() error = %v", err)
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
