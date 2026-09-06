package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type pluginHookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type pluginEvent struct {
	Matcher string            `json:"matcher"`
	Hooks   []pluginHookEntry `json:"hooks"`
}

type pluginManifest struct {
	Name  string                   `json:"name"`
	Hooks map[string][]pluginEvent `json:"hooks"`
}

type marketplaceManifest struct {
	Plugins []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"plugins"`
}

func TestClaudePluginAssetsUseQuickstartHooks(t *testing.T) {
	t.Parallel()
	root := mustRepoRoot(t)

	marketplacePath := filepath.Join(root, ".claude-plugin", "marketplace.json")
	pluginPath := filepath.Join(root, "claude-plugin", ".claude-plugin", "plugin.json")

	marketplaceBytes, err := os.ReadFile(marketplacePath)
	if err != nil {
		t.Fatalf("ReadFile(marketplace.json) error = %v", err)
	}
	pluginBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("ReadFile(plugin.json) error = %v", err)
	}

	var marketplace marketplaceManifest
	if err := json.Unmarshal(marketplaceBytes, &marketplace); err != nil {
		t.Fatalf("marketplace json parse error = %v", err)
	}
	if len(marketplace.Plugins) == 0 {
		t.Fatalf("marketplace plugins missing: %#v", marketplace)
	}
	if marketplace.Plugins[0].Name != "links" || marketplace.Plugins[0].Source != "./claude-plugin" {
		t.Fatalf("unexpected marketplace plugin entry: %#v", marketplace.Plugins[0])
	}

	var plugin pluginManifest
	if err := json.Unmarshal(pluginBytes, &plugin); err != nil {
		t.Fatalf("plugin json parse error = %v", err)
	}
	if plugin.Name != "links" {
		t.Fatalf("plugin name = %q, want links", plugin.Name)
	}

	for _, event := range []string{"SessionStart", "PreCompact"} {
		events := plugin.Hooks[event]
		if len(events) == 0 || len(events[0].Hooks) == 0 {
			t.Fatalf("hook event %s missing command hooks: %#v", event, plugin.Hooks)
		}
		if events[0].Hooks[0].Type != "command" {
			t.Fatalf("%s hook type = %q, want command", event, events[0].Hooks[0].Type)
		}
		if events[0].Hooks[0].Command != "lit quickstart --refresh" {
			t.Fatalf("%s hook command = %q, want lit quickstart --refresh", event, events[0].Hooks[0].Command)
		}
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
// isn't on PATH: it's a dev/local check, not an inner-loop dependency, the
// same posture the suite already takes toward the optional `dolt` binary.
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
