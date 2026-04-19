package templates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReturnsEmbeddedDefaultWhenNoOverride(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	content, err := Load(AgentsSectionTemplateName, t.TempDir())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !strings.Contains(content, "BEGIN LINKS INTEGRATION") {
		t.Fatalf("embedded default missing marker: %q", content)
	}

	// Absence of override must stay absence: Load must not write anything to the global path.
	globalPath := filepath.Join(xdgRoot, "links-issue-tracker", "templates", AgentsSectionTemplateName)
	if _, err := os.Stat(globalPath); !os.IsNotExist(err) {
		t.Fatalf("Load() wrote to global path %s; want absence, got err=%v", globalPath, err)
	}
}

func TestLoadReturnsEmbeddedQuickstartWhenNoOverride(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	content, err := Load(QuickstartTemplateName, t.TempDir())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !strings.Contains(content, "Agent quickstart for links issue tracking") {
		t.Fatalf("embedded quickstart default missing summary: %q", content)
	}
}

func TestLoadGlobalOverrideWins(t *testing.T) {
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

func TestNamesReturnsAllManagedTemplates(t *testing.T) {
	names := Names()
	want := map[string]bool{
		AgentsSectionTemplateName: false,
		PrePushHookTemplateName:   false,
		QuickstartTemplateName:    false,
	}
	for _, name := range names {
		if _, ok := want[name]; !ok {
			t.Fatalf("unexpected name %q", name)
		}
		want[name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("Names() missing %q", name)
		}
	}
}

func TestEmbeddedDefaultReturnsContent(t *testing.T) {
	content, err := EmbeddedDefault(QuickstartTemplateName)
	if err != nil {
		t.Fatalf("EmbeddedDefault() error = %v", err)
	}
	if !strings.Contains(string(content), "Agent quickstart for links issue tracking") {
		t.Fatalf("embedded quickstart missing summary: %q", content)
	}
}
