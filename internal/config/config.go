package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

// Load reads config from ~/.config/links-issue-tracker/config.toml and
// optionally from <workspace>/.lit/config.toml when a workspace root is given.
// A missing file is not an error; defaults are returned.
func Load(workspaceRoot ...string) (Config, error) {
	v := viper.New()

	v.SetDefault("logging.verbose", false)
	v.SetDefault("logging.file", "")
	v.SetDefault("init.install_hooks", true)
	v.SetDefault("init.install_agents", true)
	v.SetDefault("migration.auto_apply", false)
	v.SetDefault("ready.required_fields", []string{})
	v.SetDefault("quickstart.soil_mode", false)
	v.SetDefault("snapshot.retention_budget", 5)

	// [LAW:single-enforcer] Global/project config precedence is resolved once at load time.
	globalRequired, err := mergeConfigFile(v, globalConfigPath())
	if err != nil {
		return Config{}, err
	}
	projectRequired := []string{}
	if root := pathspec.New(first(workspaceRoot)); !root.IsEmpty() {
		projectRequired, err = mergeConfigFile(v, projectConfigPath(root))
		if err != nil {
			return Config{}, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	mergedRequired := append(globalRequired, projectRequired...)
	if len(mergedRequired) == 0 {
		mergedRequired = cfg.Ready.RequiredFields
	}
	cfg.Ready.RequiredFields = mergedRequired
	// [LAW:single-enforcer] snapshot.retention_budget is validated once at the
	// trust boundary; downstream callers (lit snapshots new, future migration
	// callers of dbsnapshot.Prune) trust the value is > 0.
	if cfg.Snapshot.RetentionBudget <= 0 {
		return Config{}, fmt.Errorf("config: snapshot.retention_budget must be > 0, got %d", cfg.Snapshot.RetentionBudget)
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

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
