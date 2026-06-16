package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"

	"github.com/promptctl/links-issue-tracker/internal/pathspec"
)

// Config holds user-level settings loaded from ~/.config/links-issue-tracker/config.toml.
type Config struct {
	Logging    LoggingConfig    `mapstructure:"logging"`
	Init       InitConfig       `mapstructure:"init"`
	Migration  MigrationConfig  `mapstructure:"migration"`
	Ready      ReadyConfig      `mapstructure:"ready"`
	Quickstart QuickstartConfig `mapstructure:"quickstart"`
	Snapshot   SnapshotConfig   `mapstructure:"snapshot"`
	Sync       SyncConfig       `mapstructure:"sync"`
}

type LoggingConfig struct {
	Verbose bool   `mapstructure:"verbose"`
	File    string `mapstructure:"file"`
}

type InitConfig struct {
	InstallHooks  bool `mapstructure:"install_hooks"`
	InstallAgents bool `mapstructure:"install_agents"`
}

type MigrationConfig struct {
	AutoApply bool `mapstructure:"auto_apply"`
}

type ReadyConfig struct {
	RequiredFields []string `mapstructure:"required_fields"`
}

type QuickstartConfig struct {
	SoilMode bool `mapstructure:"soil_mode"`
}

type SnapshotConfig struct {
	RetentionBudget int `mapstructure:"retention_budget"`
}

type SyncConfig struct {
	Cadence SyncCadence `mapstructure:"cadence"`
	// Receive enables the background receive worker that fast-forwards the local
	// store to the remote head after a command, so an established clone sees
	// other machines' pushed tickets without a manual `lit sync pull`. It is
	// orthogonal to Cadence (which governs sending): a clone can receive
	// regardless of how it pushes. Default true — seamless multi-machine is the
	// goal; the off switch is the documented exception. [LAW:no-mode-explosion]
	// One boolean, one default, not a second cadence enum.
	Receive bool `mapstructure:"receive"`
}

// SyncCadence selects when lit mirrors its Dolt store to the configured git
// remote. [LAW:no-mode-explosion] The set is deliberately closed to two values
// with one default; a new cadence is a new const in syncCadences with a doc
// line, never a per-command toggle or an independent boolean.
type SyncCadence string

const (
	// SyncCadenceOnPush mirrors only when the managed pre-push git hook runs
	// (one push per `git push`). This is the default and today's behavior.
	SyncCadenceOnPush SyncCadence = "on-push"
	// SyncCadenceOnChange mirrors after every mutating lit command, shrinking
	// the window where local ticket state is invisible to other clones.
	SyncCadenceOnChange SyncCadence = "on-change"
)

// syncCadences is the closed set of legal cadence values in documentation
// order. [LAW:one-source-of-truth] The default, validation, and the error
// message all derive from this one list, so they cannot drift.
var syncCadences = []SyncCadence{SyncCadenceOnPush, SyncCadenceOnChange}

func (c SyncCadence) valid() bool {
	for _, candidate := range syncCadences {
		if c == candidate {
			return true
		}
	}
	return false
}

func syncCadenceValues() string {
	parts := make([]string, len(syncCadences))
	for i, candidate := range syncCadences {
		parts[i] = string(candidate)
	}
	return strings.Join(parts, ", ")
}

const (
	globalConfigPathEnv  = "LIT_CONFIG_GLOBAL_PATH"
	projectConfigPathEnv = "LIT_CONFIG_PROJECT_PATH"
)

// ConfigDir returns the canonical directory where global config and templates live.
func ConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "links-issue-tracker")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "links-issue-tracker")
}

// layers is the config precedence chain: merged in slice order, so later
// layers override earlier ones. [LAW:one-source-of-truth] This ordering is
// the only encoding of global-vs-project precedence.
type layers []pathspec.PathSpec

func configLayers(workspaceRoot pathspec.PathSpec) layers {
	return layers{globalConfigPath(), projectConfigPath(workspaceRoot)}
}

// merge folds every layer into v in precedence order and returns the
// concatenated required-field contributions in that same order.
// [LAW:dataflow-not-control-flow] Absent layers contribute nothing as data;
// no layer is conditionally skipped.
func (l layers) merge(v *viper.Viper) ([]string, error) {
	var required []string
	for _, layer := range l {
		fields, err := mergeConfigFile(v, layer)
		if err != nil {
			return nil, err
		}
		required = append(required, fields...)
	}
	return required, nil
}

// Load reads config from ~/.config/links-issue-tracker/config.toml and from
// <workspace>/.lit/config.toml when a workspace root is present.
// A missing file is not an error; defaults are returned.
func Load(workspaceRoot pathspec.PathSpec) (Config, error) {
	v := viper.New()

	v.SetDefault("logging.verbose", false)
	v.SetDefault("logging.file", "")
	v.SetDefault("init.install_hooks", true)
	v.SetDefault("init.install_agents", true)
	v.SetDefault("migration.auto_apply", false)
	v.SetDefault("ready.required_fields", []string{})
	v.SetDefault("quickstart.soil_mode", false)
	v.SetDefault("snapshot.retention_budget", 5)
	v.SetDefault("sync.cadence", string(SyncCadenceOnPush))
	v.SetDefault("sync.receive", true)

	required, err := configLayers(workspaceRoot).merge(v)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if len(required) > 0 {
		cfg.Ready.RequiredFields = required
	}
	// [LAW:single-enforcer] snapshot.retention_budget is validated once at the
	// trust boundary; downstream callers (lit snapshots new, future migration
	// callers of dbsnapshot.Prune) trust the value is > 0.
	if cfg.Snapshot.RetentionBudget <= 0 {
		return Config{}, fmt.Errorf("config: snapshot.retention_budget must be > 0, got %d", cfg.Snapshot.RetentionBudget)
	}
	// [LAW:single-enforcer] sync.cadence is validated once at the trust
	// boundary; the one owner of sync scheduling trusts the value is a legal
	// cadence and switches on it without re-checking. [LAW:no-silent-failure]
	// An unknown value fails loudly here rather than silently falling back.
	if !cfg.Sync.Cadence.valid() {
		return Config{}, fmt.Errorf("config: sync.cadence must be one of %s, got %q", syncCadenceValues(), cfg.Sync.Cadence)
	}
	return cfg, nil
}

func globalConfigPath() pathspec.PathSpec {
	return pathspec.New(os.Getenv(globalConfigPathEnv)).
		Or(pathspec.New(ConfigDir()).Join("config.toml"))
}

func projectConfigPath(workspaceRoot pathspec.PathSpec) pathspec.PathSpec {
	return pathspec.New(os.Getenv(projectConfigPathEnv)).
		Or(workspaceRoot.Join(".lit", "config.toml"))
}

func mergeConfigFile(v *viper.Viper, path pathspec.PathSpec) ([]string, error) {
	// An absent layer contributes nothing — genuine optionality the type declares.
	if path.IsEmpty() {
		return nil, nil
	}
	fileConfig := viper.New()
	fileConfig.SetConfigFile(path.String())
	if err := fileConfig.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if errors.As(err, &notFound) || os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := v.MergeConfigMap(fileConfig.AllSettings()); err != nil {
		return nil, fmt.Errorf("merge config %s: %w", path, err)
	}
	required := fileConfig.GetStringSlice("ready.required_fields")
	required = append(required, fileConfig.GetStringSlice("required_fields")...)
	return required, nil
}
