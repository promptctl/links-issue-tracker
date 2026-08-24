package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	Claims     ClaimsConfig     `mapstructure:"claims"`
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

// ClaimsConfig tunes the read-time derivation of which checkout is working
// which lane (design-docs/work-claims.md).
type ClaimsConfig struct {
	// FreshnessWindow is T: how long a checkout's last mutation in a lane keeps
	// holding that lane. Past it the lane is available again, with provenance.
	//
	// It is the same family of staleness heuristic as the orphaned-ticket
	// threshold, applied one level up — lane instead of ticket — and it is a
	// heuristic for the same reason: there are no heartbeats and no liveness
	// probes anywhere in lit, so age is the only honest evidence that a stream
	// walked away. Repositories where humans idle over weekends may want ~72h;
	// agent-heavy ones may tighten it.
	FreshnessWindow time.Duration `mapstructure:"freshness_window"`
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
	// OwnerNotifyCmd is the owner's out-of-band channel for degraded sync state
	// (links-sync-pgct.4): a shell command lit runs when it detects a real
	// divergence or a failing push — e.g. a curl to an ntfy topic — with the
	// event's facts in LIT_NOTIFY_* environment variables. Empty (the default)
	// means no channel is configured and nothing runs. One string, not a mode:
	// what to send and where is the command's business, never lit's.
	// [LAW:no-mode-explosion]
	OwnerNotifyCmd string `mapstructure:"owner_notify_cmd"`
}

// SyncCadence selects when lit mirrors its Dolt store to the configured git
// remote. [LAW:no-mode-explosion] The set is deliberately closed to two values
// with one default; a new cadence is a new const in syncCadences with a doc
// line, never a per-command toggle or an independent boolean.
type SyncCadence string

const (
	// SyncCadenceOnPush mirrors only when the managed pre-push git hook runs
	// (one push per `git push`). Opt-in: a mutation on this cadence is only
	// durable on the remote once the user remembers to `git push` — exactly the
	// manual act whose absence stranded 25 changes in the links-sync-pgct field
	// incident. Kept for users who deliberately want to batch their network
	// traffic, never as the default a workspace falls into silently.
	SyncCadenceOnPush SyncCadence = "on-push"
	// SyncCadenceOnChange mirrors after every mutating lit command, shrinking
	// the window where local ticket state is invisible to other clones to
	// roughly zero. The default: a connected workspace's changes reach the
	// remote without a separate push step, so "durable locally" and "durable on
	// the remote" stop being two facts a human has to keep in sync by hand.
	// [LAW:one-source-of-truth] (links-sync-pgct.3)
	SyncCadenceOnChange SyncCadence = "on-change"
)

// syncCadences is the closed set of legal cadence values in documentation
// order. [LAW:one-source-of-truth] valid() and syncCadenceValues() both derive
// from this one list, so validation and the error message cannot drift from
// each other. The shipped default (Load's `v.SetDefault` call) is a separate,
// independently chosen literal — deliberately not "whichever value is first in
// this list" — so reordering this list alone never changes which cadence a
// workspace with no config file falls into.
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
	v.SetDefault("sync.cadence", string(SyncCadenceOnChange))
	v.SetDefault("sync.receive", true)
	v.SetDefault("sync.owner_notify_cmd", "")
	v.SetDefault("claims.freshness_window", "24h")

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
	// [LAW:single-enforcer] claims.freshness_window is validated once here, the
	// same trade snapshot.retention_budget makes above, so claim derivation reads
	// the window as a trusted positive duration. A zero or negative window would
	// not fail — it would quietly age out every claim the instant it was made,
	// leaving a tool that behaves as though the feature were off. That is a
	// silent wrong answer, so it fails loud instead. [LAW:no-silent-failure]
	if cfg.Claims.FreshnessWindow <= 0 {
		return Config{}, fmt.Errorf("config: claims.freshness_window must be a positive duration (e.g. 24h), got %s", cfg.Claims.FreshnessWindow)
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
