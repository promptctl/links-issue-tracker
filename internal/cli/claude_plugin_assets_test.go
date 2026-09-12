package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type marketplaceManifest struct {
	Plugins []struct {
		Name    string `json:"name"`
		Source  string `json:"source"`
		Version string `json:"version"`
	} `json:"plugins"`
}

func TestClaudeMarketplaceListsPlugin(t *testing.T) {
	t.Parallel()
	root := mustRepoRoot(t)

	marketplaceBytes, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatalf("ReadFile(marketplace.json) error = %v", err)
	}
	var marketplace marketplaceManifest
	if err := json.Unmarshal(marketplaceBytes, &marketplace); err != nil {
		t.Fatalf("marketplace json parse error = %v", err)
	}
	if len(marketplace.Plugins) == 0 {
		t.Fatalf("marketplace plugins missing: %#v", marketplace)
	}
	if marketplace.Plugins[0].Name != "lit" || marketplace.Plugins[0].Source != "./claude-plugin" {
		t.Fatalf("unexpected marketplace plugin entry: %#v", marketplace.Plugins[0])
	}
}

// TestClaudeMarketplaceVersionMatchesPlugin pins one released version across the
// two manifests that publish it. Claude Code reads the marketplace listing and
// the plugin's own manifest independently, so neither is derived from the other
// and a bump that lands in only one ships a listing misreporting the version the
// plugin declares — silently, since nothing else in the install path compares
// them. An absent version on either side is the same defect as a mismatched one:
// two empty strings agree while guarding nothing. [LAW:one-source-of-truth]
func TestClaudeMarketplaceVersionMatchesPlugin(t *testing.T) {
	t.Parallel()
	root := mustRepoRoot(t)

	marketplacePath := filepath.Join(root, ".claude-plugin", "marketplace.json")
	marketplaceBytes, err := os.ReadFile(marketplacePath)
	if err != nil {
		t.Fatalf("ReadFile(marketplace.json) error = %v", err)
	}
	var marketplace marketplaceManifest
	if err := json.Unmarshal(marketplaceBytes, &marketplace); err != nil {
		t.Fatalf("marketplace json parse error = %v", err)
	}
	if len(marketplace.Plugins) == 0 {
		t.Fatalf("marketplace plugins missing: %#v", marketplace)
	}

	pluginPath := filepath.Join(root, "claude-plugin", ".claude-plugin", "plugin.json")
	pluginBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("ReadFile(plugin.json) error = %v", err)
	}
	var plugin struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(pluginBytes, &plugin); err != nil {
		t.Fatalf("plugin json parse error = %v", err)
	}

	marketplaceVersion := marketplace.Plugins[0].Version
	if marketplaceVersion == "" || plugin.Version == "" {
		t.Fatalf("plugin version absent: %s declares %q, %s declares %q", marketplacePath, marketplaceVersion, pluginPath, plugin.Version)
	}
	if marketplaceVersion != plugin.Version {
		t.Fatalf("plugin version disagrees: %s declares %q, %s declares %q", marketplacePath, marketplaceVersion, pluginPath, plugin.Version)
	}
}

// TestClaudePluginShipsNoHooks pins that installing the plugin runs nothing on
// its own. A plugin is installed per user, so any hook it ships fires in every
// repository Claude Code opens, lit-initialized or not. Claude Code loads plugin
// hooks from plugin.json's "hooks" field and from the plugin's hooks/ directory,
// so both are checked. [LAW:behavior-not-structure]
func TestClaudePluginShipsNoHooks(t *testing.T) {
	t.Parallel()
	pluginDir := filepath.Join(mustRepoRoot(t), "claude-plugin")

	pluginBytes, err := os.ReadFile(filepath.Join(pluginDir, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatalf("ReadFile(plugin.json) error = %v", err)
	}
	var plugin map[string]json.RawMessage
	if err := json.Unmarshal(pluginBytes, &plugin); err != nil {
		t.Fatalf("plugin json parse error = %v", err)
	}
	if hooks, ok := plugin["hooks"]; ok {
		t.Fatalf("plugin.json declares hooks: %s", hooks)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "hooks")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("plugin ships a hooks directory (stat error = %v)", err)
	}
}

// TestClaudePluginShipsNextSkill pins the /next skill's home: the plugin's
// own skills/ directory, auto-discovered by the harness, never written into
// a consuming repo. [LAW:verifiable-goals]
func TestClaudePluginShipsNextSkill(t *testing.T) {
	t.Parallel()
	root := mustRepoRoot(t)

	skillPath := filepath.Join(root, "claude-plugin", "skills", "next", "SKILL.md")
	content, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", skillPath, err)
	}
	if !strings.HasPrefix(string(content), "---\nname: next\ndescription:") {
		t.Fatalf("skill frontmatter missing/misplaced at byte 0: %q", content[:min(len(content), 80)])
	}
}

// TestClaudePluginManifestsPassOfficialValidation runs the marketplace and
// plugin manifests through the real `claude plugin validate --strict`, the
// one checker that knows the manifest schema Claude Code actually loads —
// hand-modeling that schema in Go would drift the moment the CLI adds or
// retires a field. [LAW:verifiable-goals] Skips (not fails) when the CLI
// isn't on PATH: no workflow in this repo installs `claude`, so today this
// is a local-only check, not a CI gate — unlike the `dolt` binary, which CI
// actually installs (`.github/actions/install-dolt`) for the tests that use
// it as an oracle. Wiring `claude` into CI too is future work, deliberately
// left out here rather than adding a new network dependency to the suite in
// the same change that only needed a manifest tweak.
func TestClaudePluginManifestsPassOfficialValidation(t *testing.T) {
	t.Parallel()
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not on PATH; skipping manifest validation")
	}
	root := mustRepoRoot(t)

	for _, target := range []string{".", "claude-plugin"} {
		t.Run(target, func(t *testing.T) {
			cmd := exec.Command(claudeBin, "plugin", "validate", target, "--strict")
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("claude plugin validate %s --strict failed: %v\n%s", target, err, out)
			}
		})
	}
}

func mustRepoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	root := filepath.Clean(filepath.Join(cwd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root discovery failed: %v", err)
	}
	return root
}
