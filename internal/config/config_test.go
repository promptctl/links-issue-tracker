package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
)

func TestLoadDefaults(t *testing.T) {
	// Point XDG_CONFIG_HOME to a directory with no config file.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := Load(testEnv(), pathspec.PathSpec{})
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
	if cfg.Sync.HistoryWindow.ReRootAt() != 2000 || cfg.Sync.HistoryWindow.Keep() != 1000 {
		t.Fatalf("expected sync.history_bound re_root_at=2000, keep=1000 by default, got %d, %d", cfg.Sync.HistoryWindow.ReRootAt(), cfg.Sync.HistoryWindow.Keep())
	}
}

// Load builds sync.history_bound into the window it names only from two whole,
// non-negative counts that keep a commit and drop one. A negative count is the
// case a weak decode would not refuse: it would wrap to a height no push ever
// reaches, and the bound would silently never fire.
func TestLoadBuildsTheHistoryWindowOnlyFromWholeCounts(t *testing.T) {
	for _, tc := range []struct {
		name, body     string
		reRootAt, keep uint64
		refusal        string
	}{
		{"a legal pair", "re_root_at = 50\nkeep = 10", 50, 10, ""},
		{"keep alone", "keep = 10", 2000, 10, ""},
		{"a negative re_root_at", "re_root_at = -1", 0, 0, "sync.history_bound.re_root_at must be a whole number of commits, got -1 (int64)"},
		{"a negative keep", "keep = -1", 0, 0, "sync.history_bound.keep must be a whole number of commits, got -1 (int64)"},
		{"a quoted count", "re_root_at = \"2000\"", 0, 0, "sync.history_bound.re_root_at must be a whole number of commits, got \"2000\" (string)"},
		{"a whole float", "re_root_at = 2000.0", 0, 0, "sync.history_bound.re_root_at must be a whole number of commits, got 2000 (float64)"},
		{"a fractional count", "keep = 10.5", 0, 0, "sync.history_bound.keep must be a whole number of commits, got 10.5 (float64)"},
		{"a window that drops nothing", "re_root_at = 10\nkeep = 10", 0, 0, "config: sync.history_bound: a history window keeps at least one commit and fewer than it re-roots at; re_root_at 10, keep 10 is not one"},
		{"a window that keeps nothing", "keep = 0", 0, 0, "re_root_at 2000, keep 0 is not one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configDir := filepath.Join(dir, "links-issue-tracker")
			if err := os.MkdirAll(configDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[sync.history_bound]\n"+tc.body+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_CONFIG_HOME", dir)
			cfg, err := Load(testEnv(), pathspec.PathSpec{})
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("Load() error = %v; want a refusal containing %q", err, tc.refusal)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v; want re_root_at %d, keep %d", err, tc.reRootAt, tc.keep)
			}
			if cfg.Sync.HistoryWindow.ReRootAt() != tc.reRootAt || cfg.Sync.HistoryWindow.Keep() != tc.keep {
				t.Fatalf("Load() = %s; want re_root_at %d, keep %d", cfg.Sync.HistoryWindow, tc.reRootAt, tc.keep)
			}
		})
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
			cfg, err := Load(testEnv(), pathspec.PathSpec{})
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
			cfg, err := Load(testEnv(), pathspec.PathSpec{})
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

	cfg, err := Load(testEnv(), pathspec.PathSpec{})
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

	cfg, err := Load(testEnv(), pathspec.PathSpec{})
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

	cfg, err := Load(testEnv(), pathspec.PathSpec{})
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

	cfg, err := Load(testEnv(), pathspec.PathSpec{})
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

	if _, err := Load(testEnv(), pathspec.PathSpec{}); err == nil {
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
		if _, err := Load(testEnv(), pathspec.PathSpec{}); err == nil {
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

	cfg, err := Load(testEnv(), pathspec.PathSpec{})
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

	cfg, err := Load(testEnv(), pathspec.PathSpec{})
	if err != nil {
		t.Fatalf("Load(pathspec.PathSpec{}) error = %v, want a config with retired sections to load", err)
	}
	if !cfg.Quickstart.SoilMode {
		t.Fatal("expected soil_mode=true from the file that also carries retired sections")
	}
}

// The settings table is exactly the keys Load decodes into Config, plus
// claims.freshness_window, which parseFreshnessWindow reads instead of the
// struct tag. A key missing from the table would be reported as unknown while
// Load reads it; a key the table holds that Config does not decode would be
// accepted and change nothing.
func TestSettingsAreTheKeysLoadReads(t *testing.T) {
	want := []string{"claims.freshness_window", "sync.history_bound.re_root_at", "sync.history_bound.keep"}
	var decoded func(prefix string, t reflect.Type)
	decoded = func(prefix string, t reflect.Type) {
		for i := range t.NumField() {
			field := t.Field(i)
			tag := field.Tag.Get("mapstructure")
			switch {
			case tag == "-":
			case field.Type.Kind() == reflect.Struct:
				decoded(prefix+tag+".", field.Type)
			default:
				want = append(want, prefix+tag)
			}
		}
	}
	decoded("", reflect.TypeOf(Config{}))
	got := slices.Collect(maps.Keys(settings))
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("settings keys = %v, want the keys Load reads: %v", got, want)
	}
}

// UnknownKeys names every key a layer sets that is no setting, with the file
// that sets it, and passes over every key that is one, including the
// top-level required_fields spelling.
func TestUnknownKeysNamesEachKeyAndItsFile(t *testing.T) {
	globalRoot := t.TempDir()
	globalDir := filepath.Join(globalRoot, "links-issue-tracker")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	globalFile := filepath.Join(globalDir, "config.toml")
	globalContent := `
required_fields = ["title"]

[snapshot]
retention_budgt = 2
retention_budget = 3

[init]
install_hooks = false
`
	if err := os.WriteFile(globalFile, []byte(globalContent), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", globalRoot)

	workspaceRoot := t.TempDir()
	projectDir := filepath.Join(workspaceRoot, ".lit")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectFile := filepath.Join(projectDir, "config.toml")
	projectContent := `
[sync]
cadence = "on-push"
future_setting = 1

[claims]
freshness_window = "2h"
`
	if err := os.WriteFile(projectFile, []byte(projectContent), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := UnknownKeys(testEnv(), pathspec.New(workspaceRoot))
	if err != nil {
		t.Fatalf("UnknownKeys error = %v", err)
	}
	want := []UnknownKey{
		{Key: "init.install_hooks", File: globalFile},
		{Key: "snapshot.retention_budgt", File: globalFile},
		{Key: "sync.future_setting", File: projectFile},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnknownKeys = %#v, want %#v", got, want)
	}
}

// With no config file in either layer there is nothing unknown, and the list
// is empty rather than nil, so "checked and found none" encodes as [].
func TestUnknownKeysIsEmptyWithNoConfigFiles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	got, err := UnknownKeys(testEnv(), pathspec.New(t.TempDir()))
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("UnknownKeys = %#v, %v; want an empty, non-nil list", got, err)
	}
}

// A layer that does not parse cannot be checked, and the error names it; the
// other layer is still checked.
func TestUnknownKeysReportsAnUnparseableLayer(t *testing.T) {
	globalRoot := t.TempDir()
	globalFile := filepath.Join(globalRoot, "links-issue-tracker", "config.toml")
	if err := os.MkdirAll(filepath.Dir(globalFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalFile, []byte("[init]\ninstall_hooks = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", globalRoot)
	workspaceRoot := t.TempDir()
	projectFile := filepath.Join(workspaceRoot, ".lit", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectFile, []byte("[sync\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := UnknownKeys(testEnv(), pathspec.New(workspaceRoot))
	if err == nil || !strings.Contains(err.Error(), projectFile) {
		t.Fatalf("UnknownKeys error = %v, want one naming %s", err, projectFile)
	}
	if want := []UnknownKey{{Key: "init.install_hooks", File: globalFile}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UnknownKeys = %#v, want the global layer's key %#v", got, want)
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

	cfg, err := Load(testEnv(), pathspec.PathSpec{})
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

	cfg, err := Load(testEnv(), pathspec.PathSpec{})
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

	cfg, err := Load(testEnv(), pathspec.New(workspaceRoot))
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

	cfg, err := Load(testEnv(), pathspec.New(workspaceRoot))
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

	cfg, err := Load(testEnv(), pathspec.PathSpec{})
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

	if _, err := Load(testEnv(), pathspec.PathSpec{}); err == nil {
		t.Fatal("Load(pathspec.PathSpec{}) expected error for invalid TOML")
	}
}

// testEnv is the test process's environment, read when it is called, so a
// test's t.Setenv reaches the Load it runs.
func testEnv() environ.Env { return environ.Of(os.Environ()) }
