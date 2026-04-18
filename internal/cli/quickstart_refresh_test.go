package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestQuickstartRefreshRewritesManagedAssetsAndIsIdempotent(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}

	agentsPath := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("stale agents guidance\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(AGENTS.md) error = %v", err)
	}
	hookPath := filepath.Join(ws.GitCommonDir, "hooks", "pre-push")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(hooks dir) error = %v", err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/usr/bin/env bash\necho stale-hook\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(pre-push) error = %v", err)
	}

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	first := runQuickstartRefresh(t)
	if !strings.Contains(first, "refresh hooks=updated agents=updated") {
		t.Fatalf("quickstart refresh output = %q, want updated summary", first)
	}

	firstAgents, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("ReadFile(AGENTS.md) error = %v", err)
	}
	if string(firstAgents) == "stale agents guidance\n" {
		t.Fatal("quickstart --refresh should rewrite AGENTS.md")
	}

	second := runQuickstartRefresh(t)
	if !strings.Contains(second, "refresh hooks=unchanged agents=unchanged") {
		t.Fatalf("second quickstart refresh output = %q, want unchanged summary", second)
	}

	secondAgents, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("ReadFile(AGENTS.md second pass) error = %v", err)
	}
	if string(secondAgents) != string(firstAgents) {
		t.Fatal("quickstart --refresh should converge to a stable AGENTS.md rewrite")
	}
}

func TestQuickstartRefreshReportsIncompatibleHookAsSkipped(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}

	hookPath := filepath.Join(ws.GitCommonDir, "hooks", "pre-push")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(hooks dir) error = %v", err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/usr/bin/env sh\necho incompatible-hook\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(pre-push) error = %v", err)
	}

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	output := runQuickstartRefresh(t)
	if !strings.Contains(output, "refresh hooks=skipped(incompatible)") {
		t.Fatalf("quickstart refresh output = %q, want skipped(incompatible)", output)
	}
}

func runQuickstartRefresh(t *testing.T) string {
	t.Helper()
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"quickstart", "--refresh"}); err != nil {
		t.Fatalf("Run(quickstart --refresh) error = %v", err)
	}
	return stdout.String()
}
