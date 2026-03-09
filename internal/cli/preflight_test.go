package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestRunBlocksNonInitCommandsWhenBeadsResidueDetected(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("Use beads for tasks.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(AGENTS.md) error = %v", err)
	}

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	var stdout bytes.Buffer
	err = Run(context.Background(), &stdout, &stdout, []string{"ls"})
	if err == nil {
		t.Fatal("Run(ls) unexpectedly succeeded with beads residue present")
	}
	var preflightErr BeadsMigrationRequiredError
	if !errors.As(err, &preflightErr) {
		t.Fatalf("expected BeadsMigrationRequiredError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "lit migrate beads --apply --json") {
		t.Fatalf("preflight error missing remediation command: %v", err)
	}
}

func TestShouldBypassBeadsPreflight(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{args: []string{"help"}, want: true},
		{args: []string{"completion", "bash"}, want: true},
		{args: []string{"init"}, want: true},
		{args: []string{"migrate", "beads"}, want: true},
		{args: []string{"migrate"}, want: false},
		{args: []string{"migrate", "other"}, want: false},
		{args: []string{"ls"}, want: false},
	}
	for _, tc := range cases {
		got := shouldBypassBeadsPreflight(tc.args)
		if got != tc.want {
			t.Fatalf("shouldBypassBeadsPreflight(%v)=%t, want %t", tc.args, got, tc.want)
		}
	}
}

func TestRequireBeadsMigrationPreflight(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	if err := requireBeadsMigrationPreflight(ws); err != nil {
		t.Fatalf("requireBeadsMigrationPreflight() unexpected error with clean workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("beads residue\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(AGENTS.md) error = %v", err)
	}
	err = requireBeadsMigrationPreflight(ws)
	if err == nil {
		t.Fatal("requireBeadsMigrationPreflight() unexpectedly succeeded with beads residue")
	}
	var preflightErr BeadsMigrationRequiredError
	if !errors.As(err, &preflightErr) {
		t.Fatalf("expected BeadsMigrationRequiredError, got %T: %v", err, err)
	}
}
