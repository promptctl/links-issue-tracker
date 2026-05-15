package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestSnapshotsListEmptyOnFreshWorkspace verifies the read-only contract:
// a freshly-initialized workspace reports no snapshots and does not error.
func TestSnapshotsListEmptyOnFreshWorkspace(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	var initOut bytes.Buffer
	if err := Run(context.Background(), &initOut, &initOut, []string{"init", "--json"}); err != nil {
		t.Fatalf("Run(init) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"snapshots", "list"}); err != nil {
		t.Fatalf("Run(snapshots list) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "No snapshots found") {
		t.Errorf("snapshots list output = %q, want 'No snapshots found'", stdout.String())
	}
}

// TestSnapshotsListAfterSecondOpenJSON verifies that after a workspace open
// has taken at least one snapshot, the JSON list output is well-formed and
// includes the expected fields.
func TestSnapshotsListAfterSecondOpenJSON(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	// init creates the workspace (no snapshot yet).
	var initOut bytes.Buffer
	if err := Run(context.Background(), &initOut, &initOut, []string{"init", "--json"}); err != nil {
		t.Fatalf("Run(init) error = %v", err)
	}
	// Any subsequent Open creates a snapshot. `lit ready` opens the store
	// for read; that's enough to trigger the pre-Open snapshot.
	var readyOut bytes.Buffer
	_ = Run(context.Background(), &readyOut, &readyOut, []string{"ready", "--json"})

	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"snapshots", "list", "--json"}); err != nil {
		t.Fatalf("Run(snapshots list --json) error = %v", err)
	}

	var snaps []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &snaps); err != nil {
		t.Fatalf("parse snapshots list JSON error = %v\nraw: %s", err, stdout.String())
	}
	if len(snaps) == 0 {
		t.Fatal("expected at least one snapshot after Open, got 0")
	}
	for _, s := range snaps {
		for _, field := range []string{"name", "path", "created_at"} {
			if _, ok := s[field]; !ok {
				t.Errorf("snapshot JSON missing %q field: %v", field, s)
			}
		}
	}
}

// TestSnapshotsRestoreRefusesMissingSnapshot verifies the precondition
// check: restore against a non-existent snapshot name fails BEFORE any
// destructive action.
func TestSnapshotsRestoreRefusesMissingSnapshot(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	var initOut bytes.Buffer
	if err := Run(context.Background(), &initOut, &initOut, []string{"init", "--json"}); err != nil {
		t.Fatalf("Run(init) error = %v", err)
	}

	var stdout bytes.Buffer
	err = Run(context.Background(), &stdout, &stdout, []string{"snapshots", "restore", "--name", "snap-doesnt-exist"})
	if err == nil {
		t.Fatal("snapshots restore against missing name succeeded; want error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v; want a 'not found' message", err)
	}
}
