package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestSkewFloorAdvancedByMigrationGatesOlderBinary covers the core
// workspace_requires_newer_binary path through the full Open flow:
//
//  1. migration 3 is temporarily declared to require codeVersion as its
//     min_code_version (normally default=1), so advanceCompatFloor bumps
//     code_compat_floor to codeVersion.
//  2. The seam lowers the binary to codeVersion-1 for the second Open.
//  3. gate: floor (codeVersion) > binary (codeVersion-1) → typed refusal.
//
// This exercises the advanceCompatFloor→checkCompatWindow pipeline rather than
// directly tampering with the meta row (which is what compat_window_test.go already covers).
func TestSkewFloorAdvancedByMigrationGatesOlderBinary(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "skew-floor-gating-id"

	// Declare migration 3 as requiring the current codeVersion so the runner
	// advances the floor to codeVersion after it applies.
	prevMin, hadPrev := migrationMinCodeVersions[3]
	migrationMinCodeVersions[3] = codeVersion
	t.Cleanup(func() {
		if hadPrev {
			migrationMinCodeVersions[3] = prevMin
		} else {
			delete(migrationMinCodeVersions, 3)
		}
	})

	// First Open: applies migrations 1–3; advanceCompatFloor bumps floor to codeVersion.
	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	// Lower binary version to codeVersion-1 via seam.
	seam := codeVersion - 1
	testBinaryCodeVersionOverride = &seam
	t.Cleanup(func() { testBinaryCodeVersionOverride = nil })

	_, err = Open(ctx, doltRoot, wsID)
	var compatErr *CompatError
	if !errors.As(err, &compatErr) {
		t.Fatalf("expected *CompatError, got %T: %v", err, err)
	}
	if compatErr.Reason != "workspace_requires_newer_binary" {
		t.Fatalf("Reason = %q, want %q", compatErr.Reason, "workspace_requires_newer_binary")
	}
	if compatErr.WorkspaceCompatFloor != codeVersion {
		t.Fatalf("WorkspaceCompatFloor = %d, want %d", compatErr.WorkspaceCompatFloor, codeVersion)
	}
	if compatErr.BinaryCodeVersion != seam {
		t.Fatalf("BinaryCodeVersion = %d, want %d", compatErr.BinaryCodeVersion, seam)
	}
}

// TestSkewAppliesInjectedPendingMigrationWhenInWindow covers the runner's
// happy path for advancing a workspace through a pending migration that
// would (after applying) keep the workspace within the binary's
// compat-window:
//   - first Open settles at real migrations 1–3 (top=3, codeVersion=3,
//     in-window).
//   - the test seam registers an extra Go migration at pendingVersion and
//     lifts the effective binary codeVersion to pendingVersion so the gate
//     accepts the post-apply state.
//   - second Open applies the pending migration successfully and ends with
//     goose_db_version=pendingVersion.
//
// (The earlier framing of "DB behind binary" was misleading — the version
// numbers here exist only as test injections, not as a real older-vs-newer
// binary scenario. checkCompatWindow refuses workspaces whose goose top
// exceeds codeVersion, so the override is necessary to keep the second
// Open's post-apply state in-window.)
func TestSkewAppliesInjectedPendingMigrationWhenInWindow(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "skew-advance-id"
	const pendingVersion int64 = 99993

	// First Open: workspace at real migrations 1–3; no pendingVersion yet.
	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	// Register v99993 as a pending successful migration.
	t.Cleanup(installSuccessfulMigration(pendingVersion))
	// Override binary codeVersion so the post-apply state (top=99993) is
	// still in-window — otherwise checkCompatWindow refuses the second Open.
	override := pendingVersion
	prevOverride := testBinaryCodeVersionOverride
	testBinaryCodeVersionOverride = &override
	t.Cleanup(func() { testBinaryCodeVersionOverride = prevOverride })

	// Second Open: runner sees v99993 pending → applies it → succeeds.
	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("second Open() (with pending migration) error = %v", err)
	}
	defer second.Close()

	requireGooseVersionPresent(t, ctx, second, int(pendingVersion))
}

// TestSkewNoMigrationsWhenCurrentVersion covers the DB-at-binary-version case:
// a workspace already at the current version emits no migrate.commit events on
// the next Open.
func TestSkewNoMigrationsWhenCurrentVersion(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "skew-current-id"

	// First Open: workspace reaches current version.
	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	var buf bytes.Buffer
	t.Cleanup(restoreEventWriter(&buf))

	// Second Open: no pending migrations — no commit events.
	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer second.Close()

	committed := parseMigrateCommitEvents(t, buf.String())
	if len(committed) != 0 {
		t.Fatalf("expected 0 migrate.commit events on second open, got %d: %v", len(committed), committed)
	}
}

// TestSkewFloorBelowDBVersionDoesNotGate covers the negative case: a workspace
// where code_compat_floor < goose_db_version (floor not advanced to match the
// DB's highest version) still opens successfully, validating that the gate is
// floor-vs-binary, not floor-vs-top.
//
// With no migrationMinCodeVersions overrides, all migrations default to
// min_code_version=1, so floor is advanced to 1 while goose_db_version reaches 3.
// Both satisfy floor ≤ binary AND top ≤ binary.
func TestSkewFloorBelowDBVersionDoesNotGate(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "skew-floor-below-id"

	// First Open: applies migrations 1–3. floor = 1 (max min_code_version),
	// goose_db_version = 3.
	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	// Second Open: floor(1) < top(3) ≤ codeVersion(3) → succeeds.
	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("second Open() error = %v (gate must not fire when floor < top ≤ binary)", err)
	}
	defer second.Close()

	// Confirm the precondition: floor is genuinely below the DB version.
	floor, err := readCodeCompatFloor(ctx, second.db)
	if err != nil {
		t.Fatalf("readCodeCompatFloor error = %v", err)
	}
	top, err := readGooseDBVersion(ctx, second.db)
	if err != nil {
		t.Fatalf("readGooseDBVersion error = %v", err)
	}
	if floor >= top {
		t.Fatalf("precondition failed: want floor(%d) < top(%d)", floor, top)
	}
}
