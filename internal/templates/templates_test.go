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

func TestLoadWithSourceReportsLayer(t *testing.T) {
	t.Run("embedded when no overrides", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())

		content, source, err := LoadWithSource(AgentsSectionTemplateName, t.TempDir())
		if err != nil {
			t.Fatalf("LoadWithSource() error = %v", err)
		}
		if source != SourceEmbedded {
			t.Fatalf("LoadWithSource() source = %q, want %q", source, SourceEmbedded)
		}
		if !strings.Contains(content, "BEGIN LINKS INTEGRATION") {
			t.Fatalf("embedded default missing marker: %q", content)
		}
	})

	t.Run("global override", func(t *testing.T) {
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

		content, source, err := LoadWithSource(AgentsSectionTemplateName, t.TempDir())
		if err != nil {
			t.Fatalf("LoadWithSource() error = %v", err)
		}
		if content != want {
			t.Fatalf("LoadWithSource() content = %q, want %q", content, want)
		}
		if source != SourceGlobal {
			t.Fatalf("LoadWithSource() source = %q, want %q", source, SourceGlobal)
		}
	})

	t.Run("project override masks global", func(t *testing.T) {
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

		content, source, err := LoadWithSource(AgentsSectionTemplateName, workspaceRoot)
		if err != nil {
			t.Fatalf("LoadWithSource() error = %v", err)
		}
		if content != want {
			t.Fatalf("LoadWithSource() content = %q, want %q", content, want)
		}
		if source != SourceProject {
			t.Fatalf("LoadWithSource() source = %q, want %q", source, SourceProject)
		}
	})
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


