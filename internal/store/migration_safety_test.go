package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestRunnerAutoRevertsBrokenMigration covers the spec acceptance: a
// migration body that errors → runner auto-reverts, returns *MigrationError,
// workspace at pre-migration state, version is quarantined, subsequent
// Opens skip it.
//
// We synthesize the failure with a Go migration registered via the
// extraMigrationProviderOptions test seam so the test does not depend on a
// real bad SQL file shipping in migrations/.
func TestRunnerAutoRevertsBrokenMigration(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "auto-revert-id"

	// First Open with no extras: workspace lands on baseline + 00002.
	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	// Capture event emissions for assertion.
	var buf bytes.Buffer
	t.Cleanup(restoreEventWriter(&buf))

	const badVersion int64 = 99
	t.Cleanup(installBadMigration(badVersion, errors.New("synthetic test failure")))

	// Second Open: bad version pending → auto-revert + quarantine.
	second, err := Open(ctx, doltRoot, wsID)
	if err == nil {
		_ = second.Close()
		t.Fatalf("expected second Open to fail with MigrationError, got success")
	}
	var migErr *MigrationError
	if !errors.As(err, &migErr) {
		t.Fatalf("expected *MigrationError, got %T: %v", err, err)
	}
	if migErr.Phase != "up" {
		t.Errorf("expected MigrationError.Phase=up, got %q", migErr.Phase)
	}
	if migErr.Version != badVersion {
		t.Errorf("expected MigrationError.Version=%d, got %d", badVersion, migErr.Version)
	}

	emitted := buf.String()
	for _, want := range []string{"safety_branch.created", "migrate.failed", "safety_branch.reverted"} {
		if !strings.Contains(emitted, want) {
			t.Errorf("expected event %q in stderr, got:\n%s", want, emitted)
		}
	}

	// Re-open WITHOUT the bad migration registered: workspace is back to
	// pre-bad-migration state and the quarantine row records the failure.
	t.Cleanup(installBadMigration(0, nil)) // no-op; the previous Cleanup restores
	extraMigrationProviderOptions = nil

	third, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("third Open() (post-revert) error = %v", err)
	}
	defer third.Close()

	quarantined, err := readQuarantinedVersions(ctx, third.db)
	if err != nil {
		t.Fatalf("readQuarantinedVersions error = %v", err)
	}
	if len(quarantined) != 1 || quarantined[0] != badVersion {
		t.Fatalf("expected quarantine to record version %d, got %v", badVersion, quarantined)
	}

	// And the bad version is NOT applied in goose_db_version.
	var applied int
	if err := third.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+gooseVersionTable+" WHERE version_id = ? AND is_applied = TRUE",
		badVersion).Scan(&applied); err != nil {
		t.Fatalf("query goose version error = %v", err)
	}
	if applied != 0 {
		t.Fatalf("expected bad version %d NOT to appear applied; found %d row(s)", badVersion, applied)
	}
}

// TestQuarantinedVersionIsSkippedOnNextOpen covers the spec acceptance:
// workspace with quarantined version → next Open emits
// migrate.skipped_quarantined and succeeds without re-invoking the
// quarantined version.
//
// Note on test scope: goose.WithExcludeVersions filters migrations sourced
// from the embedded filesystem, not those registered via WithGoMigrations
// (this is goose's design, not ours). Production migrations are SQL files,
// so the production guarantee holds — the test below covers the runner's
// wiring (read quarantine, emit per-version event, hand exclude list to
// provider, Open succeeds), and trusts goose's filesystem-source filter as
// covered by goose's own test suite.
func TestQuarantinedVersionIsSkippedOnNextOpen(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "skip-quarantined-id"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}

	const quarantinedVersion int64 = 77
	if err := first.withCommitLock(ctx, func(ctx context.Context) error {
		if err := first.recordQuarantine(ctx, quarantinedVersion, "seed for skip test"); err != nil {
			return err
		}
		return first.commitWorkingSet(ctx, "seed quarantine")
	}); err != nil {
		t.Fatalf("seed quarantine error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	var buf bytes.Buffer
	t.Cleanup(restoreEventWriter(&buf))

	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("second Open() error = %v (quarantined-version Open should succeed)", err)
	}
	defer second.Close()

	emitted := buf.String()
	want := fmt.Sprintf("migrate.skipped_quarantined version=%d", quarantinedVersion)
	if !strings.Contains(emitted, want) {
		t.Errorf("expected event %q in stderr, got:\n%s", want, emitted)
	}

	// Read the quarantine to confirm the row survived the second Open's
	// migration path (it must — runner reads but never writes quarantine
	// during the success path).
	quarantined, err := readQuarantinedVersions(ctx, second.db)
	if err != nil {
		t.Fatalf("readQuarantinedVersions error = %v", err)
	}
	if len(quarantined) != 1 || quarantined[0] != quarantinedVersion {
		t.Fatalf("expected quarantine to retain version %d after Open, got %v", quarantinedVersion, quarantined)
	}
}

// TestSmokeTestFailsWithRecoveryHint covers the spec acceptance: a workspace
// whose schema is intentionally broken (e.g., a column dropped manually
// post-Open) → smoke test fails, output names the recovery command.
func TestSmokeTestFailsWithRecoveryHint(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Drop a column the smoke probe selects so the next Doctor pass
	// surfaces the breakage.
	if _, err := st.db.ExecContext(ctx, "ALTER TABLE issues DROP COLUMN topic"); err != nil {
		t.Fatalf("synthesize broken schema error = %v", err)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if report.SmokeTest == "ok" {
		t.Fatalf("expected SmokeTest != ok after column drop, got %q", report.SmokeTest)
	}
	if !strings.Contains(report.SmokeTest, "smoke test \"issues\" failed") {
		t.Errorf("expected smoke test message to name failed probe, got %q", report.SmokeTest)
	}
	if !strings.Contains(report.SmokeTest, "lit doctor --reset-to-pre-migration") {
		t.Errorf("expected smoke test message to name recovery command, got %q", report.SmokeTest)
	}
	if len(report.Errors) == 0 {
		t.Fatalf("expected smoke failure to populate report.Errors, got none")
	}
}

// TestResetToPreMigrationRestoresAndQuarantines covers the spec acceptance
// for the manual recovery flow: synthesize a "successful" migration applied
// after a checkpoint exists, run ResetToPreMigration, verify the workspace
// reverts and the version is quarantined so subsequent Opens skip it.
func TestResetToPreMigrationRestoresAndQuarantines(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "reset-to-pre-migration-id"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	// Register a successful Go migration at version 88. Open applies it.
	const recoverVersion int64 = 88
	t.Cleanup(installSuccessfulMigration(recoverVersion))

	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	// Confirm the migration applied.
	requireGooseVersionPresent(t, ctx, second, int(recoverVersion))

	// Now run the recovery surface.
	result, err := second.ResetToPreMigration(ctx)
	if err != nil {
		t.Fatalf("ResetToPreMigration error = %v", err)
	}
	if len(result.QuarantinedVersions) != 1 || result.QuarantinedVersions[0] != recoverVersion {
		t.Fatalf("expected quarantine to record %d, got %v", recoverVersion, result.QuarantinedVersions)
	}
	if !strings.HasPrefix(result.Checkpoint, preMigrateCheckpointPrefix+"-") {
		t.Errorf("expected checkpoint name to start with %q, got %q", preMigrateCheckpointPrefix, result.Checkpoint)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	// Subsequent Open with no extras: workspace at pre-version-88 state,
	// quarantine retains 88 so the recovery is durable.
	extraMigrationProviderOptions = nil

	third, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("third Open() (post-recovery) error = %v", err)
	}
	defer third.Close()

	quarantined, err := readQuarantinedVersions(ctx, third.db)
	if err != nil {
		t.Fatalf("readQuarantinedVersions error = %v", err)
	}
	found := false
	for _, v := range quarantined {
		if v == recoverVersion {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected version %d in quarantine after recovery, got %v", recoverVersion, quarantined)
	}
}

// TestResetToPreMigrationFailsWhenNoCheckpoint surfaces the agent-facing
// sentinel rather than letting a generic "branch not found" bubble out.
func TestResetToPreMigrationFailsWhenNoCheckpoint(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Strip every pre-migrate checkpoint so the recovery has nothing to
	// land on.
	cps, err := st.ListCheckpoints(ctx, preMigrateCheckpointPrefix)
	if err != nil {
		t.Fatalf("ListCheckpoints error = %v", err)
	}
	for _, cp := range cps {
		if _, err := st.db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", cp.Name); err != nil {
			t.Fatalf("delete checkpoint %s error = %v", cp.Name, err)
		}
	}

	if _, err := st.ResetToPreMigration(ctx); !errors.Is(err, ErrNoPreMigrationCheckpoint) {
		t.Fatalf("expected ErrNoPreMigrationCheckpoint, got %v", err)
	}
}

// installBadMigration registers a Go migration that returns the given error.
// Returns a Cleanup function that resets the test seam. Pass version=0 with
// nil error to install a no-op cleanup (used for chained cleanup).
func installBadMigration(version int64, failWith error) func() {
	if version == 0 {
		return func() {}
	}
	prev := extraMigrationProviderOptions
	extraMigrationProviderOptions = func() []goose.ProviderOption {
		return []goose.ProviderOption{
			goose.WithGoMigrations(
				goose.NewGoMigration(version, &goose.GoFunc{
					RunTx: func(ctx context.Context, tx *sql.Tx) error {
						return failWith
					},
				}, nil),
			),
		}
	}
	return func() { extraMigrationProviderOptions = prev }
}

// installSuccessfulMigration registers a Go migration that succeeds with no
// schema changes — enough for goose to record it as applied so the recovery
// flow has something to quarantine.
func installSuccessfulMigration(version int64) func() {
	prev := extraMigrationProviderOptions
	extraMigrationProviderOptions = func() []goose.ProviderOption {
		return []goose.ProviderOption{
			goose.WithGoMigrations(
				goose.NewGoMigration(version, &goose.GoFunc{
					RunTx: func(ctx context.Context, tx *sql.Tx) error { return nil },
				}, nil),
			),
		}
	}
	return func() { extraMigrationProviderOptions = prev }
}

// restoreEventWriter swaps migrationEventWriter for the test buffer and
// returns the cleanup that restores stderr. Tests use t.Cleanup with this.
func restoreEventWriter(buf *bytes.Buffer) func() {
	prev := migrationEventWriter
	migrationEventWriter = buf
	return func() { migrationEventWriter = prev }
}
