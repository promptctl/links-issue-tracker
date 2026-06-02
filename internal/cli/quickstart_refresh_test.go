package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/templates"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
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
	if !strings.Contains(first, "Refreshed: pre-push hook, AGENTS.md (via embedded), CLAUDE.md (via embedded)") {
		t.Fatalf("quickstart refresh output = %q, want refreshed summary", first)
	}

	firstAgents, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("ReadFile(AGENTS.md) error = %v", err)
	}
	if string(firstAgents) == "stale agents guidance\n" {
		t.Fatal("quickstart --refresh should rewrite AGENTS.md")
	}

	second := runQuickstartRefresh(t)
	if !strings.Contains(second, "Up to date: pre-push hook, AGENTS.md (via embedded), CLAUDE.md (via embedded)") {
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
	if !strings.Contains(output, "Skipped: pre-push hook (incompatible)") {
		t.Fatalf("quickstart refresh output = %q, want skipped incompatible", output)
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
	if !strings.Contains(output, "Skipped: quickstart template (customized)") {
		t.Fatalf("quickstart refresh output = %q, want quickstart skipped customized", output)
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
	if !strings.Contains(output, "Up to date: quickstart template") {
		t.Fatalf("quickstart refresh output = %q, want quickstart unchanged", output)
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
	if !strings.Contains(output, "Up to date: quickstart template") {
		t.Fatalf("quickstart refresh output = %q, want quickstart unchanged (project layer wins)", output)
	}
}

func TestQuickstartRefreshShowsGlobalSourceForAgentsSection(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	repo := t.TempDir()
	runGit(t, repo, "init")

	embedded, err := templates.EmbeddedDefault(templates.AgentsSectionTemplateName)
	if err != nil {
		t.Fatalf("EmbeddedDefault() error = %v", err)
	}
	globalPath := filepath.Join(xdg, "links-issue-tracker", "templates", templates.AgentsSectionTemplateName)
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(global templates) error = %v", err)
	}
	if err := os.WriteFile(globalPath, embedded, 0o644); err != nil {
		t.Fatalf("WriteFile(global override) error = %v", err)
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
	if !strings.Contains(output, "AGENTS.md (via global)") {
		t.Fatalf("quickstart refresh output = %q, want agents source = via global", output)
	}
	if !strings.Contains(output, "CLAUDE.md (via global)") {
		t.Fatalf("quickstart refresh output = %q, want claude source = via global", output)
	}
}

func TestQuickstartRefreshShowsProjectSourceForAgentsSection(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	repo := t.TempDir()
	runGit(t, repo, "init")

	embedded, err := templates.EmbeddedDefault(templates.AgentsSectionTemplateName)
	if err != nil {
		t.Fatalf("EmbeddedDefault() error = %v", err)
	}
	projectPath := filepath.Join(repo, ".lit", "templates", templates.AgentsSectionTemplateName)
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
	if !strings.Contains(output, "AGENTS.md (via project)") {
		t.Fatalf("quickstart refresh output = %q, want agents source = via project", output)
	}
	if !strings.Contains(output, "CLAUDE.md (via project)") {
		t.Fatalf("quickstart refresh output = %q, want claude source = via project", output)
	}
}

func TestRenderQuickstartGuidanceAppendsSoilSectionWhenEnabled(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	configDir := filepath.Join(xdg, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(config dir) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[quickstart]\nsoil_mode = true\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(config.toml) error = %v", err)
	}

	repo := t.TempDir()
	runGit(t, repo, "init")

	got, err := renderQuickstartGuidance(repo)
	if err != nil {
		t.Fatalf("renderQuickstartGuidance() error = %v", err)
	}
	if !strings.Contains(got, "## Soil") {
		t.Fatalf("renderQuickstartGuidance() with soil_mode=true: output missing Soil section\ngot: %s", got)
	}
	if !strings.Contains(got, "[SOIL:") {
		t.Fatalf("renderQuickstartGuidance() with soil_mode=true: output missing SOIL marker syntax\ngot: %s", got)
	}

	// Verify default (no config) does not include the soil section.
	xdg2 := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg2)
	gotDefault, err := renderQuickstartGuidance(repo)
	if err != nil {
		t.Fatalf("renderQuickstartGuidance() default error = %v", err)
	}
	if strings.Contains(gotDefault, "## Soil") {
		t.Fatalf("renderQuickstartGuidance() with soil_mode=false (default): output must not include Soil section\ngot: %s", gotDefault)
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
