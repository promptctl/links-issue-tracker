package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
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
