package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/templates"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestQuickstartRefreshRewritesManagedAssetsAndIsIdempotent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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
	if !strings.Contains(first, "refresh hooks=updated agents=updated quickstart=absent") {
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
	if !strings.Contains(second, "refresh hooks=unchanged agents=unchanged quickstart=absent") {
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
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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

func TestQuickstartRefreshReportsStaleGlobalOverrideAsCustomizedWithoutOverwriting(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	repo := t.TempDir()
	runGit(t, repo, "init")

	globalPath := filepath.Join(xdg, "links-issue-tracker", "templates", templates.QuickstartTemplateName)
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(global templates) error = %v", err)
	}
	staleContent := []byte("# stale verbatim copy from before --reason flag changes\n")
	if err := os.WriteFile(globalPath, staleContent, 0o644); err != nil {
		t.Fatalf("WriteFile(stale override) error = %v", err)
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
	if !strings.Contains(output, "quickstart=skipped(customized)") {
		t.Fatalf("quickstart refresh output = %q, want quickstart=skipped(customized)", output)
	}

	got, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatalf("ReadFile(global override) error = %v", err)
	}
	if string(got) != string(staleContent) {
		t.Fatalf("refresh must not overwrite a customized override; got %q, want %q", got, staleContent)
	}
}

func TestQuickstartRefreshReportsCurrentGlobalOverrideAsUnchanged(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	repo := t.TempDir()
	runGit(t, repo, "init")

	embedded, err := templates.EmbeddedDefault(templates.QuickstartTemplateName)
	if err != nil {
		t.Fatalf("EmbeddedDefault() error = %v", err)
	}
	globalPath := filepath.Join(xdg, "links-issue-tracker", "templates", templates.QuickstartTemplateName)
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(global templates) error = %v", err)
	}
	if err := os.WriteFile(globalPath, embedded, 0o644); err != nil {
		t.Fatalf("WriteFile(current override) error = %v", err)
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
	if !strings.Contains(output, "quickstart=unchanged") {
		t.Fatalf("quickstart refresh output = %q, want quickstart=unchanged", output)
	}
}

func TestQuickstartRefreshProjectOverrideMasksGlobal(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	repo := t.TempDir()
	runGit(t, repo, "init")

	embedded, err := templates.EmbeddedDefault(templates.QuickstartTemplateName)
	if err != nil {
		t.Fatalf("EmbeddedDefault() error = %v", err)
	}

	// Stale global; current project. Project layer wins, so refresh must report unchanged.
	globalPath := filepath.Join(xdg, "links-issue-tracker", "templates", templates.QuickstartTemplateName)
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(global templates) error = %v", err)
	}
	if err := os.WriteFile(globalPath, []byte("stale global\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(global override) error = %v", err)
	}
	projectPath := filepath.Join(repo, ".lit", "templates", templates.QuickstartTemplateName)
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(project templates) error = %v", err)
	}
	if err := os.WriteFile(projectPath, embedded, 0o644); err != nil {
		t.Fatalf("WriteFile(project override) error = %v", err)
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
	if !strings.Contains(output, "quickstart=unchanged") {
		t.Fatalf("quickstart refresh output = %q, want quickstart=unchanged (project layer wins)", output)
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
