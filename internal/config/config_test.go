package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/pathspec"
)

func TestLoadDefaults(t *testing.T) {
	// Point XDG_CONFIG_HOME to a directory with no config file.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := Load(pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("Load(pathspec.PathSpec{}) error = %v", err)
	}

	if len(cfg.Ready.RequiredFields) != 0 {
		t.Fatalf("expected no required fields by default, got %#v", cfg.Ready.RequiredFields)
	}
	if cfg.Quickstart.SoilMode {
		t.Fatal("expected soil_mode=false by default")
	}
	if cfg.Snapshot.RetentionBudget != 5 {
		t.Fatalf("expected snapshot.retention_budget=5 by default, got %d", cfg.Snapshot.RetentionBudget)
	}
	if cfg.Sync.Cadence != SyncCadenceOnChange {
		t.Fatalf("expected sync.cadence=on-change by default, got %q", cfg.Sync.Cadence)
	}
	if !cfg.Sync.Receive {
		t.Fatal("expected sync.receive=true by default (seamless multi-machine), got false")
	}
	if cfg.Sync.OwnerNotifyCmd != "" {
		t.Fatalf("expected sync.owner_notify_cmd empty by default (no channel configured), got %q", cfg.Sync.OwnerNotifyCmd)
	}
	if cfg.Claims.FreshnessWindow != 6*time.Hour {
		t.Fatalf("expected claims.freshness_window=6h by default, got %s", cfg.Claims.FreshnessWindow)
	}
}

// TestLoadRejectsUnusableFreshnessWindow covers both ways the window can arrive
// unusable, because only one of them looks wrong.
//
// A bare number is the mistake this file invites: every other numeric setting
// here is a plain integer, so `freshness_window = 72` reads as "72 hours" to a
// human and weak-decodes to 72 *nanoseconds* through viper. It is positive, so
// no bounds check can catch it — a window that expires every claim the instant
// it is made, with nothing to say the feature is off. The only thing that
// separates it from a deliberate value is that it was never a duration, which is
// why the key is parsed rather than bounds-checked.
func TestLoadRejectsUnusableFreshnessWindow(t *testing.T) {
	cases := map[string]string{
		"bare number that reads like hours": "[claims]\nfreshness_window = 72\n",
		"bare zero":                         "[claims]\nfreshness_window = 0\n",
		"quoted number with no unit":        "[claims]\nfreshness_window = \"72\"\n",
		"zero duration":                     "[claims]\nfreshness_window = \"0s\"\n",
		"negative duration":                 "[claims]\nfreshness_window = \"-1h\"\n",
		"not a duration at all":             "[claims]\nfreshness_window = \"soon\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			configDir := filepath.Join(dir, "links-issue-tracker")
			if err := os.MkdirAll(configDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_CONFIG_HOME", dir)
			cfg, err := Load(pathspec.PathSpec{})
			if err == nil {
				t.Fatalf("Load() with %q accepted the value as %s, want a refusal", body, cfg.Claims.FreshnessWindow)
			}
		})
	}
}

// TestLoadAcceptsDurationsWithUnits: the refusal above must not have been bought
// by rejecting everything.
func TestLoadAcceptsDurationsWithUnits(t *testing.T) {
	cases := map[string]time.Duration{
		"\"72h\"":    72 * time.Hour,
		"\"90m\"":    90 * time.Minute,
		"\"1h30m\"":  90 * time.Minute,
		"\"30s\"":    30 * time.Second,
		"\"1000ms\"": time.Second,
	}
	for literal, want := range cases {
		t.Run(literal, func(t *testing.T) {
			dir := t.TempDir()
			configDir := filepath.Join(dir, "links-issue-tracker")
			if err := os.MkdirAll(configDir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := "[claims]\nfreshness_window = " + literal + "\n"
			if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_CONFIG_HOME", dir)
			cfg, err := Load(pathspec.PathSpec{})
			if err != nil {
				t.Fatalf("Load() with %q error = %v", body, err)
			}
			if cfg.Claims.FreshnessWindow != want {
				t.Fatalf("claims.freshness_window = %s, want %s", cfg.Claims.FreshnessWindow, want)
			}
		})
	}
}

func TestLoadSyncOwnerNotifyCmdFromTOML(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := "[sync]\nowner_notify_cmd = 'curl -s -d \"$LIT_NOTIFY_SUMMARY\" https://ntfy.example/lit'\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg, err := Load(pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("Load(pathspec.PathSpec{}) error = %v", err)
	}
	if want := `curl -s -d "$LIT_NOTIFY_SUMMARY" https://ntfy.example/lit`; cfg.Sync.OwnerNotifyCmd != want {
		t.Fatalf("sync.owner_notify_cmd = %q, want %q", cfg.Sync.OwnerNotifyCmd, want)
	}
}

func TestLoadSyncReceiveFromTOML(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[sync]\nreceive = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg, err := Load(pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("Load(pathspec.PathSpec{}) error = %v", err)
	}
	if cfg.Sync.Receive {
		t.Fatal("expected sync.receive=false from file, got true")
	}
}

func TestLoadSyncCadenceFromTOML(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[sync]\ncadence = \"on-change\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg, err := Load(pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("Load(pathspec.PathSpec{}) error = %v", err)
	}
	if cfg.Sync.Cadence != SyncCadenceOnChange {
		t.Fatalf("expected sync.cadence=on-change from file, got %q", cfg.Sync.Cadence)
	}
}

// TestLoadSyncCadenceOnPushFromTOML pins the opt-out path: a user who
// deliberately wants manual push control must still be able to select on-push
// from file, and Load must not silently keep the default when a file
// explicitly asks for the other value.
func TestLoadSyncCadenceOnPushFromTOML(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[sync]\ncadence = \"on-push\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg, err := Load(pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("Load(pathspec.PathSpec{}) error = %v", err)
	}
	if cfg.Sync.Cadence != SyncCadenceOnPush {
		t.Fatalf("expected sync.cadence=on-push from file, got %q", cfg.Sync.Cadence)
	}
}

func TestLoadRejectsUnknownSyncCadence(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[sync]\ncadence = \"hourly\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	if _, err := Load(pathspec.PathSpec{}); err == nil {
		t.Fatal("Load(pathspec.PathSpec{}) with unknown sync.cadence expected error, got nil")
	}
}

func TestLoadRejectsNonPositiveRetentionBudget(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		"[snapshot]\nretention_budget = 0\n",
		"[snapshot]\nretention_budget = -1\n",
	}
	for _, body := range cases {
		if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_CONFIG_HOME", dir)
		if _, err := Load(pathspec.PathSpec{}); err == nil {
			t.Fatalf("Load(pathspec.PathSpec{}) with body %q expected error, got nil", body)
		}
	}
}

func TestLoadFromTOML(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `
[ready]
required_fields = ["description"]

[quickstart]
soil_mode = true
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg, err := Load(pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("Load(pathspec.PathSpec{}) error = %v", err)
	}

	if !reflect.DeepEqual(cfg.Ready.RequiredFields, []string{"description"}) {
		t.Fatalf("required fields = %#v, want [description]", cfg.Ready.RequiredFields)
	}
	if !cfg.Quickstart.SoilMode {
		t.Fatal("expected soil_mode=true from file")
	}
}

// A config written for an older lit can still carry sections lit no longer
// declares; it loads, and the keys lit does declare beside them still apply.
func TestLoadAcceptsRetiredSections(t *testing.T) {
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

[quickstart]
soil_mode = true
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg, err := Load(pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("Load(pathspec.PathSpec{}) error = %v, want a config with retired sections to load", err)
	}
	if !cfg.Quickstart.SoilMode {
		t.Fatal("expected soil_mode=true from the file that also carries retired sections")
	}
}

func TestLoadPartialTOML(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only set the quickstart section; every other section should get defaults.
	content := `
[quickstart]
soil_mode = true
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg, err := Load(pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("Load(pathspec.PathSpec{}) error = %v", err)
	}

	if !cfg.Quickstart.SoilMode {
		t.Fatal("expected soil_mode=true from file")
	}
	if !cfg.Sync.Receive {
		t.Fatal("expected sync.receive=true from default")
	}
	if cfg.Snapshot.RetentionBudget != 5 {
		t.Fatalf("expected snapshot.retention_budget=5 from default, got %d", cfg.Snapshot.RetentionBudget)
	}
	if len(cfg.Ready.RequiredFields) != 0 {
		t.Fatalf("expected no required fields, got %#v", cfg.Ready.RequiredFields)
	}
}

func TestLoadMissingDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/nonexistent/path/that/does/not/exist")

	cfg, err := Load(pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("Load(pathspec.PathSpec{}) error = %v", err)
	}

	// Should return defaults without error.
	if !cfg.Sync.Receive {
		t.Fatal("expected sync.receive=true from default")
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

	cfg, err := Load(pathspec.New(workspaceRoot))
	if err != nil {
		t.Fatalf("Load(pathspec.New(workspaceRoot)) error = %v", err)
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
[sync]
receive = true
owner_notify_cmd = "global-notify"
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
[sync]
receive = false
`
	if err := os.WriteFile(filepath.Join(projectConfigDir, "config.toml"), []byte(projectContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(pathspec.New(workspaceRoot))
	if err != nil {
		t.Fatalf("Load(pathspec.New(workspaceRoot)) error = %v", err)
	}
	if cfg.Sync.Receive {
		t.Fatal("expected project sync.receive=false to override global")
	}
	if cfg.Sync.OwnerNotifyCmd != "global-notify" {
		t.Fatalf("expected global sync.owner_notify_cmd to remain set, got %q", cfg.Sync.OwnerNotifyCmd)
	}
}

func TestLoadHonorsProjectEnvOverrideWithoutWorkspaceRoot(t *testing.T) {
	// The project layer is part of the precedence chain whether or not a
	// workspace root is present; an explicit env override is never silently
	// dropped. Pins the layer chain as data, not a conditional merge.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	projectFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(projectFile, []byte("[quickstart]\nsoil_mode = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIT_CONFIG_PROJECT_PATH", projectFile)

	cfg, err := Load(pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Quickstart.SoilMode {
		t.Fatal("expected soil_mode=true from env-overridden project layer")
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

	if _, err := Load(pathspec.PathSpec{}); err == nil {
		t.Fatal("Load(pathspec.PathSpec{}) expected error for invalid TOML")
	}
}

// The data home is XDG_DATA_HOME when it is set and ~/.local/share otherwise,
// the base cc-candybar reads installed plugins from.
func TestDataHomeFollowsXDGDataHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	if got, err := DataHome(); err != nil || got != filepath.Join(home, ".local", "share") {
		t.Fatalf("DataHome with XDG_DATA_HOME unset = %q, %v; want %s", got, err, filepath.Join(home, ".local", "share"))
	}
	t.Setenv("XDG_DATA_HOME", "/xdg/data")
	if got, err := DataHome(); err != nil || got != "/xdg/data" {
		t.Fatalf("DataHome = %q, %v; want /xdg/data", got, err)
	}
}
