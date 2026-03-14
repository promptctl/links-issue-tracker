package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// Point XDG_CONFIG_HOME to a directory with no config file.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Logging.Verbose {
		t.Fatal("expected verbose=false by default")
	}
	if cfg.Logging.File != "" {
		t.Fatalf("expected empty log file by default, got %q", cfg.Logging.File)
	}
	if !cfg.Init.InstallHooks {
		t.Fatal("expected install_hooks=true by default")
	}
	if !cfg.Init.InstallAgents {
		t.Fatal("expected install_agents=true by default")
	}
	if cfg.Migration.AutoApply {
		t.Fatal("expected auto_apply=false by default")
	}
	if len(cfg.Ready.RequiredFields) != 0 {
		t.Fatalf("expected no required fields by default, got %#v", cfg.Ready.RequiredFields)
	}
}

func TestLoadFromTOML(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `
[logging]
verbose = true
file = "/tmp/lit.log"

[init]
install_hooks = false
install_agents = false

[migration]
auto_apply = true

[ready]
required_fields = ["description"]
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !cfg.Logging.Verbose {
		t.Fatal("expected verbose=true")
	}
	if cfg.Logging.File != "/tmp/lit.log" {
		t.Fatalf("expected file=/tmp/lit.log, got %q", cfg.Logging.File)
	}
	if cfg.Init.InstallHooks {
		t.Fatal("expected install_hooks=false")
	}
	if cfg.Init.InstallAgents {
		t.Fatal("expected install_agents=false")
	}
	if !cfg.Migration.AutoApply {
		t.Fatal("expected auto_apply=true")
	}
	if !reflect.DeepEqual(cfg.Ready.RequiredFields, []string{"description"}) {
		t.Fatalf("required fields = %#v, want [description]", cfg.Ready.RequiredFields)
	}
}

func TestLoadPartialTOML(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only set logging section; init and migration should get defaults.
	content := `
[logging]
verbose = true
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !cfg.Logging.Verbose {
		t.Fatal("expected verbose=true from file")
	}
	if !cfg.Init.InstallHooks {
		t.Fatal("expected install_hooks=true from default")
	}
	if !cfg.Init.InstallAgents {
		t.Fatal("expected install_agents=true from default")
	}
	if cfg.Migration.AutoApply {
		t.Fatal("expected auto_apply=false from default")
	}
	if len(cfg.Ready.RequiredFields) != 0 {
		t.Fatalf("expected no required fields, got %#v", cfg.Ready.RequiredFields)
	}
}

func TestLoadMissingDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/nonexistent/path/that/does/not/exist")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Should return defaults without error.
	if !cfg.Init.InstallHooks {
		t.Fatal("expected install_hooks=true from default")
	}
}

func TestLoadMergesGlobalAndProjectRequiredFields(t *testing.T) {
	globalRoot := t.TempDir()
	globalConfigDir := filepath.Join(globalRoot, "links-issue-tracker")
	if err := os.MkdirAll(globalConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	globalContent := `
[ready]
required_fields = ["description", "assignee"]
`
	if err := os.WriteFile(filepath.Join(globalConfigDir, "config.toml"), []byte(globalContent), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", globalRoot)

	workspaceRoot := t.TempDir()
	projectConfigDir := filepath.Join(workspaceRoot, ".lit")
	if err := os.MkdirAll(projectConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectContent := `
[ready]
required_fields = ["title", "description"]
`
	if err := os.WriteFile(filepath.Join(projectConfigDir, "config.toml"), []byte(projectContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(workspaceRoot)
	if err != nil {
		t.Fatalf("Load(workspaceRoot) error = %v", err)
	}
	want := []string{"description", "assignee", "title", "description"}
	if !reflect.DeepEqual(cfg.Ready.RequiredFields, want) {
		t.Fatalf("required fields = %#v, want %#v", cfg.Ready.RequiredFields, want)
	}
}

func TestLoadGlobalAndProjectOverrides(t *testing.T) {
	globalRoot := t.TempDir()
	globalConfigDir := filepath.Join(globalRoot, "links-issue-tracker")
	if err := os.MkdirAll(globalConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	globalContent := `
[logging]
verbose = false
file = "/tmp/global.log"
`
	if err := os.WriteFile(filepath.Join(globalConfigDir, "config.toml"), []byte(globalContent), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", globalRoot)

	workspaceRoot := t.TempDir()
	projectConfigDir := filepath.Join(workspaceRoot, ".lit")
	if err := os.MkdirAll(projectConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectContent := `
[logging]
verbose = true
`
	if err := os.WriteFile(filepath.Join(projectConfigDir, "config.toml"), []byte(projectContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(workspaceRoot)
	if err != nil {
		t.Fatalf("Load(workspaceRoot) error = %v", err)
	}
	if !cfg.Logging.Verbose {
		t.Fatal("expected project logging.verbose=true to override global")
	}
	if cfg.Logging.File != "/tmp/global.log" {
		t.Fatalf("expected global log file to remain set, got %q", cfg.Logging.File)
	}
}

func TestLoadInvalidTOMLReturnsError(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[ready\nrequired_fields = [\"description\"]"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for invalid TOML")
	}
}
