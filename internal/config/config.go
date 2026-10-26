package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/historywindow"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
)

// Config holds user-level settings loaded from ~/.config/links-issue-tracker/config.toml.
type Config struct {
	Ready      ReadyConfig      `mapstructure:"ready"`
	Quickstart QuickstartConfig `mapstructure:"quickstart"`
	Snapshot   SnapshotConfig   `mapstructure:"snapshot"`
	Sync       SyncConfig       `mapstructure:"sync"`
	Claims     ClaimsConfig     `mapstructure:"claims"`
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
	// threshold, one level up — lane instead of ticket — and a heuristic for the
	// same reason: lit has no heartbeats and no liveness probes, so age is the
	// only honest evidence that a stream walked away. [LAW:one-source-of-truth]
	// It shares that threshold's value, not its subject (abandoned claim vs.
	// abandoned ticket), so the two stay separate constants, free to move apart.
	//
	// Filled by parseFreshnessWindow rather than by struct-tag decoding, which
	// is why the tag excludes it. Every other numeric setting in this file is a
	// bare integer, so `freshness_window = 72` is the mistake a reader of this
	// file is most likely to make — and viper's weak decoding would land it in
	// the Duration's underlying int64 as 72 *nanoseconds*, a positive value that
	// passes validation and expires every claim instantly.
	FreshnessWindow time.Duration `mapstructure:"-"`
}

// parseFreshnessWindow turns the raw claims.freshness_window value into a
// duration, and it is the reason that key is not decoded by struct tag. Its wire
// type is a duration *string* — "72h", "90m" — and viper's decode hook only
// converts a string source, so a bare number silently bypasses it and weak-decodes
// into nanoseconds. Reading the raw value as a string and parsing it here means a
// bare `72` fails as "missing unit in duration" instead of arriving as a plausible
// number that no later check can tell apart from a deliberate one.
//
// [LAW:parse-dont-validate] Validating that the number is positive never
// establishes that it is a duration at all, which is exactly the question that
// matters: 72ns is positive. One crossing, and past it the value is a duration
// because parsing is what produced it. [LAW:no-silent-failure] Both failure
// arms — unparseable, and parseable but non-positive — are loud.
func parseFreshnessWindow(raw string) (time.Duration, error) {
	window, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: claims.freshness_window must be a duration with a unit, like \"72h\" or \"90m\" (got %q): %w", raw, err)
	}
	if window <= 0 {
		return 0, fmt.Errorf("config: claims.freshness_window must be positive, got %s", window)
	}
	return window, nil
}

// parseHistoryWindow is sync.history_bound as the window it names, refused
// when either count is not a whole number of commits or the pair keeps none
// or drops none. A TOML integer, and the default, are int64; anything else is
// a value the user did not mean as a count. [LAW:parse-dont-validate]
func parseHistoryWindow(v *viper.Viper) (historywindow.Window, error) {
	count := func(key string) (uint64, error) {
		n, ok := v.Get(key).(int64)
		if !ok || n < 0 {
			return 0, fmt.Errorf("config: %s must be a whole number of commits, got %#v (%T)", key, v.Get(key), v.Get(key))
		}
		return uint64(n), nil
	}
	reRootAt, err := count("sync.history_bound.re_root_at")
	if err != nil {
		return historywindow.Window{}, err
	}
	keep, err := count("sync.history_bound.keep")
	if err != nil {
		return historywindow.Window{}, err
	}
	window, err := historywindow.New(reRootAt, keep)
	if err != nil {
		return historywindow.Window{}, fmt.Errorf("config: sync.history_bound: %w", err)
	}
	return window, nil
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
	// OwnerNotifyCmd is the owner's out-of-band channel for degraded sync state:
	// a shell command lit runs when it detects a real divergence or a failing
	// push — e.g. a curl to an ntfy topic — with the event's facts in
	// LIT_NOTIFY_* environment variables. Empty (the default) means no channel
	// is configured and nothing runs. One string, not a mode: what to send and
	// where is the command's business, never lit's.
	// [LAW:no-mode-explosion]
	OwnerNotifyCmd string `mapstructure:"owner_notify_cmd"`
	// HistoryWindow is sync.history_bound: the window the sync branch's
	// history is kept inside. A push that leaves the remote head at height
	// re_root_at or above re-roots it, keeping keep commits. It is what sets a
	// store's size, since the store grows with every commit it keeps. The
	// defaults hold this repository's store between 35.8 MB and 64.7 MB,
	// measured on a copy of it (links-storage-3k6u.8ka). Filled by
	// parseHistoryWindow rather than by struct-tag decoding, whose weak decode
	// would take re_root_at = -1 as a height no push ever reaches.
	HistoryWindow historywindow.Window `mapstructure:"-"`
}

// SyncCadence selects when lit mirrors its Dolt store to the configured git
// remote. [LAW:no-mode-explosion] The set is deliberately closed to two values
// with one default; a new cadence is a new const in syncCadences with a doc
// line, never a per-command toggle or an independent boolean.
type SyncCadence string

const (
	// SyncCadenceOnPush mirrors only when the managed pre-push git hook runs
	// (one push per `git push`). Opt-in: a mutation on this cadence is only
	// durable on the remote once the user remembers to `git push`. Kept for
	// users who deliberately want to batch their network traffic, never as the
	// default a workspace falls into silently.
	SyncCadenceOnPush SyncCadence = "on-push"
	// SyncCadenceOnChange mirrors after every mutating lit command, shrinking
	// the window where local ticket state is invisible to other clones to
	// roughly zero. The default: a connected workspace's changes reach the
	// remote without a separate push step, so "durable locally" and "durable on
	// the remote" stop being two facts a human has to keep in sync by hand.
	// [LAW:one-source-of-truth]
	SyncCadenceOnChange SyncCadence = "on-change"
)

// syncCadences is the closed set of legal cadence values in documentation
// order. [LAW:one-source-of-truth] valid() and syncCadenceValues() both derive
// from this one list, so validation and the error message cannot drift from
// each other. The shipped default (sync.cadence in the settings table) is a
// separate, independently chosen literal — deliberately not "whichever value is
// first in this list" — so reordering this list alone never changes which
// cadence a workspace with no config file falls into.
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

// appDirName is lit's directory under each XDG base directory.
// [LAW:one-source-of-truth]
const appDirName = "links-issue-tracker"

// ConfigDir returns the canonical directory where global config and templates live.
func ConfigDir(env environ.Env) string {
	if xdg := env.Get("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appDirName)
	}
	home, err := env.HomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", appDirName)
}

// StateDir returns the directory for lit's per-user state that outlives one
// invocation but is no project's data: XDG_STATE_HOME, by default
// ~/.local/state.
func StateDir(env environ.Env) (string, error) {
	if xdg := env.Get("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, appDirName), nil
	}
	home, err := env.HomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve state directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", appDirName), nil
}

// settings is every key a config layer can set, with the value it takes when
// no layer sets it. [LAW:one-source-of-truth] Load's defaults and the keys
// UnknownKeys accepts are both this table, and TestSettingsAreTheKeysLoadReads
// holds it to the keys Load decodes, so a key cannot be read without a default
// or accepted without being read.
var settings = map[string]any{
	"ready.required_fields":         []string{},
	"quickstart.soil_mode":          false,
	"snapshot.retention_budget":     5,
	"sync.cadence":                  string(SyncCadenceOnChange),
	"sync.receive":                  true,
	"sync.owner_notify_cmd":         "",
	"sync.history_bound.re_root_at": int64(2000),
	"sync.history_bound.keep":       int64(1000),
	"claims.freshness_window":       "6h",
}

// legacyRequiredFieldsKey is the top-level spelling of ready.required_fields.
// It has no default of its own: each layer's value is appended to that layer's
// ready.required_fields list by mergeConfigFile.
const legacyRequiredFieldsKey = "required_fields"

// isSetting reports whether a key a config layer sets is one Load reads.
func isSetting(key string) bool {
	_, ok := settings[key]
	return ok || key == legacyRequiredFieldsKey
}

// UnknownKey is a key a config file sets that is no setting of lit's, so
// setting it changes nothing: a misspelling, a removed setting, or one a newer
// lit added.
type UnknownKey struct {
	Key  string `json:"key"`
	File string `json:"file"`
}

// UnknownKeys lists the keys each config layer sets that are no setting, in
// precedence order and alphabetically within a file. Load does not refuse
// them: the project layer is committed and read by clones on older lits, so
// refusing a key a newer lit added would break every command on those clones.
// Keys are as viper reads them, lowercased, which is also how Load matches
// them to settings. Each layer is its own file, so a layer that does not parse
// adds its error and hides nothing another layer sets.
func UnknownKeys(env environ.Env, project ProjectLayer) ([]UnknownKey, error) {
	unknown := []UnknownKey{}
	var errs []error
	for _, layer := range configLayers(env, project) {
		file, err := readLayer(layer)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		keys := file.AllKeys()
		slices.Sort(keys)
		for _, key := range keys {
			if !isSetting(key) {
				unknown = append(unknown, UnknownKey{Key: key, File: layer.String()})
			}
		}
	}
	return unknown, errors.Join(errs...)
}

// layer is one config file of the precedence chain: its content, which
// readLayer parses, and its name for messages.
type layer interface {
	// Content is the layer's bytes: a layer with no file has none.
	Content() ([]byte, error)
	String() string
}

// layers is the config precedence chain: merged in slice order, so later
// layers override earlier ones. [LAW:one-source-of-truth] This ordering is
// the only encoding of global-vs-project precedence.
type layers []layer

func configLayers(env environ.Env, project ProjectLayer) layers {
	return layers{fileLayer{globalConfigPath(env)}, project}
}

// ProjectLayer is the project config layer, read above the global one. It is
// either a checkout's, the file at LIT_CONFIG_PROJECT_PATH or else the
// checkout's .lit/config.toml, read when Load runs; or the content a detached
// worker was handed by the command that launched it, whose checkout may be
// gone by the time the worker reads config. [LAW:types-are-the-program] a
// worker's layer is a value it holds, so no config read on its path can reach
// for a checkout that no longer exists.
type ProjectLayer interface {
	// Content is what a launcher hands its worker.
	layer
	// Source names where the layer came from — checkout:<path> or handed —
	// for the command's event: which file won is a fact of every command.
	// [LAW:nothing-unseen]
	Source() string
}

// CheckoutProject is the project layer of the checkout at workspaceRoot, the
// file LIT_CONFIG_PROJECT_PATH names taking its place. An empty root has no
// file of its own, but the variable still names one.
func CheckoutProject(env environ.Env, workspaceRoot pathspec.PathSpec) ProjectLayer {
	return fileLayer{projectConfigPath(env, workspaceRoot)}
}

// HandedProject is a project layer handed over as content, read from no file.
func HandedProject(content []byte) ProjectLayer {
	return handedLayer{content: content}
}

// fileLayer is a layer read from the file at path when Load runs.
type fileLayer struct{ path pathspec.PathSpec }

func (l fileLayer) String() string { return l.path.String() }

func (l fileLayer) Source() string { return "checkout:" + l.path.String() }

// Content is the file's bytes: no path, or no file at it, is a layer that
// sets nothing.
func (l fileLayer) Content() ([]byte, error) {
	if l.path.IsEmpty() {
		return nil, nil
	}
	content, err := os.ReadFile(l.path.String())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", l.path, err)
	}
	return content, nil
}

// NoProject is the project layer of a worker that was handed none: it sets
// nothing, and says so on the event, where "handed" would claim a handoff
// that never arrived. [FRAMING:representation]
func NoProject() ProjectLayer { return noProjectLayer{} }

type noProjectLayer struct{}

func (noProjectLayer) Content() ([]byte, error) { return nil, nil }

func (noProjectLayer) String() string { return "no project config" }

func (noProjectLayer) Source() string { return "none" }

// handedLayer is a project layer that arrived as content.
type handedLayer struct{ content []byte }

func (l handedLayer) String() string { return "handed project config" }

func (l handedLayer) Source() string { return "handed" }

func (l handedLayer) Content() ([]byte, error) { return l.content, nil }

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
// the project layer above it. A missing file is not an error; defaults are
// returned.
func Load(env environ.Env, project ProjectLayer) (Config, error) {
	v := viper.New()
	for key, value := range settings {
		v.SetDefault(key, value)
	}

	required, err := configLayers(env, project).merge(v)
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
	// [LAW:single-enforcer] claims.freshness_window becomes a duration once, here,
	// so claim derivation reads the window as a trusted positive duration and
	// never re-checks it. A window that survived as zero, negative, or
	// nanosecond-scale would not fail — it would quietly age out every claim the
	// instant it was made, leaving a tool that behaves as though the feature were
	// off. GetString rather than GetDuration: viper's cast would repeat the weak
	// decode this exists to refuse. [LAW:no-silent-failure]
	window, err := parseFreshnessWindow(v.GetString("claims.freshness_window"))
	if err != nil {
		return Config{}, err
	}
	cfg.Claims.FreshnessWindow = window
	// [LAW:single-enforcer] sync.history_bound becomes a historywindow.Window
	// once, here, so the push path is handed a window that keeps and drops and
	// never re-checks it.
	historyWindow, err := parseHistoryWindow(v)
	if err != nil {
		return Config{}, err
	}
	cfg.Sync.HistoryWindow = historyWindow
	return cfg, nil
}

func globalConfigPath(env environ.Env) pathspec.PathSpec {
	return pathspec.New(env.Get(globalConfigPathEnv)).
		Or(pathspec.New(ConfigDir(env)).Join("config.toml"))
}

func projectConfigPath(env environ.Env, workspaceRoot pathspec.PathSpec) pathspec.PathSpec {
	return pathspec.New(env.Get(projectConfigPathEnv)).
		Or(workspaceRoot.Join(".lit", "config.toml"))
}

func mergeConfigFile(v *viper.Viper, layer layer) ([]string, error) {
	fileConfig, err := readLayer(layer)
	if err != nil {
		return nil, err
	}
	if err := v.MergeConfigMap(fileConfig.AllSettings()); err != nil {
		return nil, fmt.Errorf("merge config %s: %w", layer, err)
	}
	required := fileConfig.GetStringSlice("ready.required_fields")
	required = append(required, fileConfig.GetStringSlice(legacyRequiredFieldsKey)...)
	return required, nil
}

// readLayer parses one config layer's content as TOML, the format every
// layer is documented in. A file and the same bytes handed to a worker are
// one parse, so a launcher and its worker cannot read one layer two ways; a
// layer with no content sets nothing. [LAW:one-source-of-truth]
func readLayer(l layer) (*viper.Viper, error) {
	content, err := l.Content()
	if err != nil {
		return nil, err
	}
	file := viper.New()
	file.SetConfigType("toml")
	if err := file.ReadConfig(bytes.NewReader(content)); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", l, err)
	}
	return file, nil
}
