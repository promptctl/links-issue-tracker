package templates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLoadResetsMissingGlobalTemplate(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	workspaceRoot := t.TempDir()
	content, err := Load(AgentsSectionTemplateName, workspaceRoot)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !strings.Contains(content, "BEGIN LINKS INTEGRATION") {
		t.Fatalf("loaded template missing marker: %q", content)
	}

	globalPath := filepath.Join(xdgRoot, "links-issue-tracker", "templates", AgentsSectionTemplateName)
	globalContent, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatalf("ReadFile(global template) error = %v", err)
	}
	if len(globalContent) == 0 {
		t.Fatal("global template should be reset to non-empty default")
	}
}

func TestLoadResetsInvalidGlobalTemplateWhenEmpty(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	globalTemplates := filepath.Join(xdgRoot, "links-issue-tracker", "templates")
	if err := os.MkdirAll(globalTemplates, 0o755); err != nil {
		t.Fatalf("MkdirAll(global templates) error = %v", err)
	}
	globalPath := filepath.Join(globalTemplates, AgentsSectionTemplateName)
	if err := os.WriteFile(globalPath, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile(empty global template) error = %v", err)
	}

	content, err := Load(AgentsSectionTemplateName, t.TempDir())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !strings.Contains(content, "BEGIN LINKS INTEGRATION") {
		t.Fatalf("loaded template missing marker: %q", content)
	}

	rewritten, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatalf("ReadFile(rewritten global template) error = %v", err)
	}
	if len(rewritten) == 0 {
		t.Fatal("global template remained empty after Load")
	}
}

func TestLoadResetsInvalidGlobalTemplateWhenNotUTF8(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	globalTemplates := filepath.Join(xdgRoot, "links-issue-tracker", "templates")
	if err := os.MkdirAll(globalTemplates, 0o755); err != nil {
		t.Fatalf("MkdirAll(global templates) error = %v", err)
	}
	globalPath := filepath.Join(globalTemplates, PrePushHookTemplateName)
	if err := os.WriteFile(globalPath, []byte{0xff, 0xfe, 0xfd}, 0o644); err != nil {
		t.Fatalf("WriteFile(non-utf8 global template) error = %v", err)
	}

	content, err := Load(PrePushHookTemplateName, t.TempDir())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !strings.Contains(content, "BEGIN LINKS INTEGRATION") {
		t.Fatalf("loaded template missing marker: %q", content)
	}

	rewritten, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatalf("ReadFile(rewritten global template) error = %v", err)
	}
	if !utf8.Valid(rewritten) {
		t.Fatalf("rewritten global template is not valid utf8: %v", rewritten)
	}
}

func TestLoadGlobalOverride(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	globalTemplates := filepath.Join(xdgRoot, "links-issue-tracker", "templates")
	if err := os.MkdirAll(globalTemplates, 0o755); err != nil {
		t.Fatalf("MkdirAll(global templates) error = %v", err)
	}
	want := "custom global template\n"
	if err := os.WriteFile(filepath.Join(globalTemplates, AgentsSectionTemplateName), []byte(want), 0o644); err != nil {
		t.Fatalf("WriteFile(global template) error = %v", err)
	}

	got, err := Load(AgentsSectionTemplateName, t.TempDir())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got != want {
		t.Fatalf("Load() = %q, want %q", got, want)
	}
}

func TestLoadProjectOverrideWins(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	globalTemplates := filepath.Join(xdgRoot, "links-issue-tracker", "templates")
	if err := os.MkdirAll(globalTemplates, 0o755); err != nil {
		t.Fatalf("MkdirAll(global templates) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(globalTemplates, AgentsSectionTemplateName), []byte("global\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(global template) error = %v", err)
	}

	workspaceRoot := t.TempDir()
	projectTemplates := filepath.Join(workspaceRoot, ".lit", "templates")
	if err := os.MkdirAll(projectTemplates, 0o755); err != nil {
		t.Fatalf("MkdirAll(project templates) error = %v", err)
	}
	want := "project\n"
	if err := os.WriteFile(filepath.Join(projectTemplates, AgentsSectionTemplateName), []byte(want), 0o644); err != nil {
		t.Fatalf("WriteFile(project template) error = %v", err)
	}

	got, err := Load(AgentsSectionTemplateName, workspaceRoot)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got != want {
		t.Fatalf("Load() = %q, want %q", got, want)
	}
}

func TestLoadPropagatesProjectFilesystemError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	workspaceRoot := t.TempDir()
	projectTemplatePath := filepath.Join(workspaceRoot, ".lit", "templates", AgentsSectionTemplateName)
	if err := os.MkdirAll(projectTemplatePath, 0o755); err != nil {
		t.Fatalf("MkdirAll(project template path as dir) error = %v", err)
	}

	_, err := Load(AgentsSectionTemplateName, workspaceRoot)
	if err == nil {
		t.Fatal("Load() expected filesystem error, got nil")
	}
}

func TestLoadFailsWhenGlobalResetCannotWriteTemplate(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	configRoot := filepath.Join(xdgRoot, "links-issue-tracker")
	if err := os.MkdirAll(configRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(config root) error = %v", err)
	}
	blockedTemplatesPath := filepath.Join(configRoot, "templates")
	if err := os.WriteFile(blockedTemplatesPath, []byte("not-a-dir\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(blocking templates path) error = %v", err)
	}

	_, err := Load(AgentsSectionTemplateName, t.TempDir())
	if err == nil {
		t.Fatal("Load() expected error when global reset cannot write template")
	}
	if !strings.Contains(err.Error(), "could not write template") {
		t.Fatalf("error = %v, want write-template context", err)
	}
}

func TestSeedGlobalDefaultsIdempotent(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	if err := SeedGlobalDefaults(); err != nil {
		t.Fatalf("SeedGlobalDefaults(first) error = %v", err)
	}

	templatesDir := filepath.Join(xdgRoot, "links-issue-tracker", "templates")
	hookPath := filepath.Join(templatesDir, PrePushHookTemplateName)
	custom := "custom hook\n"
	if err := os.WriteFile(hookPath, []byte(custom), 0o644); err != nil {
		t.Fatalf("WriteFile(custom hook) error = %v", err)
	}

	if err := SeedGlobalDefaults(); err != nil {
		t.Fatalf("SeedGlobalDefaults(second) error = %v", err)
	}

	content, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("ReadFile(hook template) error = %v", err)
	}
	if string(content) != custom {
		t.Fatalf("seed overwrote existing file: got %q, want %q", string(content), custom)
	}
}

func TestSeedGlobalDefaultsCreatesDirAndFiles(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	templatesDir := filepath.Join(xdgRoot, "links-issue-tracker", "templates")
	if _, err := os.Stat(templatesDir); !os.IsNotExist(err) {
		t.Fatalf("templates dir precondition failed: err=%v", err)
	}

	if err := SeedGlobalDefaults(); err != nil {
		t.Fatalf("SeedGlobalDefaults() error = %v", err)
	}

	for _, name := range []string{AgentsSectionTemplateName, PrePushHookTemplateName} {
		path := filepath.Join(templatesDir, name)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, err)
		}
		if len(content) == 0 {
			t.Fatalf("seeded template %s is empty", path)
		}
	}
}
