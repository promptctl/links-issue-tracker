package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestCompatWindowRefusesWorkspaceRequiringNewerBinary verifies the
// "workspace_requires_newer_binary" refusal: a workspace whose
// code_compat_floor exceeds the binary's codeVersion cannot be opened.
//
// Setup: open a fresh workspace, manually write a code_compat_floor far
// above codeVersion, then reopen — the second Open must refuse.
func TestCompatWindowRefusesWorkspaceRequiringNewerBinary(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "compat-floor-too-high"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if _, err := first.db.ExecContext(ctx,
		`INSERT INTO meta (meta_key, meta_value) VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE meta_value = VALUES(meta_value)`,
		codeCompatFloorMetaKey, "999"); err != nil {
		t.Fatalf("write code_compat_floor error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "test: synthesize too-new workspace"); err != nil {
		t.Fatalf("commit error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	_, err = Open(ctx, doltRoot, wsID)
	var compatErr *CompatError
	if !errors.As(err, &compatErr) {
		t.Fatalf("expected *CompatError, got %T: %v", err, err)
	}
	if compatErr.Reason != "workspace_requires_newer_binary" {
		t.Fatalf("Reason = %q, want %q", compatErr.Reason, "workspace_requires_newer_binary")
	}
	if compatErr.WorkspaceCompatFloor != 999 {
		t.Fatalf("WorkspaceCompatFloor = %d, want 999", compatErr.WorkspaceCompatFloor)
	}
	if compatErr.BinaryCodeVersion != codeVersion {
		t.Fatalf("BinaryCodeVersion = %d, want %d", compatErr.BinaryCodeVersion, codeVersion)
	}
}

// TestCompatWindowRefusesWorkspaceAheadOfBinary verifies the
// "workspace_ahead_of_binary" refusal: a workspace whose goose_db_version
// MAX exceeds the binary's codeVersion cannot be opened.
//
// Setup: open a fresh workspace (stamps version 1), manually insert a
// goose_db_version row at version 999, then reopen — the second Open must
// refuse.
func TestCompatWindowRefusesWorkspaceAheadOfBinary(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "compat-db-version-too-high"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if _, err := first.db.ExecContext(ctx,
		"INSERT INTO "+gooseVersionTable+" (version_id, is_applied) VALUES (?, ?)",
		999, true); err != nil {
		t.Fatalf("insert future goose row error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "test: synthesize ahead-of-binary workspace"); err != nil {
		t.Fatalf("commit error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	_, err = Open(ctx, doltRoot, wsID)
	var compatErr *CompatError
	if !errors.As(err, &compatErr) {
		t.Fatalf("expected *CompatError, got %T: %v", err, err)
	}
	if compatErr.Reason != "workspace_ahead_of_binary" {
		t.Fatalf("Reason = %q, want %q", compatErr.Reason, "workspace_ahead_of_binary")
	}
	if compatErr.WorkspaceDBVersion != 999 {
		t.Fatalf("WorkspaceDBVersion = %d, want 999", compatErr.WorkspaceDBVersion)
	}
	if compatErr.BinaryCodeVersion != codeVersion {
		t.Fatalf("BinaryCodeVersion = %d, want %d", compatErr.BinaryCodeVersion, codeVersion)
	}
}

// TestCompatWindowAllowsInWindowWorkspace verifies the happy path: a
// workspace whose code_compat_floor and goose_db_version are both at or
// below the binary's codeVersion opens normally. Re-asserts the regression
// guarantee: the gate does not break ordinary Opens.
func TestCompatWindowAllowsInWindowWorkspace(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "compat-in-window"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("second Open() error = %v (want no error: workspace is in-window)", err)
	}
	defer second.Close()
}

// TestCompatWindowRefusesAtFloorBoundary verifies the inequality is strict:
// floor == codeVersion is allowed, floor == codeVersion + 1 is refused.
// Pins down the boundary so a future off-by-one doesn't slip through.
func TestCompatWindowRefusesAtFloorBoundary(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "compat-boundary"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	// Write floor exactly at codeVersion. Allowed.
	if _, err := first.db.ExecContext(ctx,
		`INSERT INTO meta (meta_key, meta_value) VALUES (?, ?)
		 ON DUPLICATE KEY UPDATE meta_value = VALUES(meta_value)`,
		codeCompatFloorMetaKey, "1"); err != nil {
		t.Fatalf("write floor=1 error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "test: floor at boundary"); err != nil {
		t.Fatalf("commit error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	st, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("Open() at floor==codeVersion error = %v (boundary should be inclusive)", err)
	}
	// Bump floor one above codeVersion. Refused.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE meta SET meta_value = ? WHERE meta_key = ?`,
		"2", codeCompatFloorMetaKey); err != nil {
		t.Fatalf("bump floor=2 error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "test: floor above boundary"); err != nil {
		t.Fatalf("commit error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("st Close() error = %v", err)
	}
	_, err = Open(ctx, doltRoot, wsID)
	var compatErr *CompatError
	if !errors.As(err, &compatErr) {
		t.Fatalf("expected *CompatError at floor=2, got %T: %v", err, err)
	}
}
