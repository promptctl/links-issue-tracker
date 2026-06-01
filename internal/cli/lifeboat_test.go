package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

// seedWorkspace creates a real current-baseline workspace at the canonical path
// and closes it, so a recover run can dump it below the gate and rebuild it.
func seedWorkspace(t *testing.T) workspace.Info {
	t.Helper()
	root := t.TempDir()
	canonical := filepath.Join(root, "dolt")
	st, err := store.Open(context.Background(), canonical, "test-workspace-id")
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close seed workspace: %v", err)
	}
	return workspace.Info{
		RootDir:      root,
		DatabasePath: canonical,
		WorkspaceID:  "test-workspace-id",
		IssuePrefix:  "test",
	}
}

// TestRunLifeboatRecoverPromotesRecognizedWorkspace is the CLI acceptance for the
// autonomous path: a recognized workspace recovers with no human input, the
// rebuild is promoted in place, and the prior contents are preserved as a backup.
func TestRunLifeboatRecoverPromotesRecognizedWorkspace(t *testing.T) {
	ws := seedWorkspace(t)
	var out bytes.Buffer

	if err := runLifeboatRecover(context.Background(), &out, ws, nil); err != nil {
		t.Fatalf("runLifeboatRecover: %v", err)
	}
	if !strings.Contains(out.String(), "recovered:") {
		t.Fatalf("expected a recovery confirmation, got: %q", out.String())
	}

	// The canonical path still holds a readable workspace, and a backup was kept.
	entries, err := os.ReadDir(ws.RootDir)
	if err != nil {
		t.Fatalf("read storage dir: %v", err)
	}
	var sawDolt, sawBackup bool
	for _, e := range entries {
		if e.Name() == "dolt" {
			sawDolt = true
		}
		if strings.HasPrefix(e.Name(), "dolt.backup-") {
			sawBackup = true
		}
	}
	if !sawDolt || !sawBackup {
		t.Fatalf("want canonical dolt dir and a backup; entries=%v", entries)
	}
}

// TestRunLifeboatRecoverRejectsExtraArgs guards the verb's argument contract.
func TestRunLifeboatRecoverRejectsExtraArgs(t *testing.T) {
	ws := seedWorkspace(t)
	var out bytes.Buffer
	if err := runLifeboatRecover(context.Background(), &out, ws, []string{"unexpected"}); err == nil {
		t.Fatal("expected a usage error for extra arguments")
	}
}
