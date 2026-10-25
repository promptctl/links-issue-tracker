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

	"github.com/promptctl/links-issue-tracker/internal/testoracle"
)

type marketplaceManifest struct {
	Plugins []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
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
// own skills/ directory, auto-discovered by the harness. [LAW:verifiable-goals]
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

// TestClaudePluginQuickstartSkillCarriesTheOpeningInstruction pins that the
// quickstart skill's description is what tells an agent in a lit repository to
// run `lit quickstart` first: the harness loads descriptions into every
// session, so the instruction lives there rather than in a file lit writes
// into the repository. [LAW:verifiable-goals]
func TestClaudePluginQuickstartSkillCarriesTheOpeningInstruction(t *testing.T) {
	t.Parallel()
	skillPath := filepath.Join(mustRepoRoot(t), "claude-plugin", "skills", "quickstart", "SKILL.md")
	content, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", skillPath, err)
	}
	if !strings.HasPrefix(string(content), "---\nname: quickstart\ndescription:") {
		t.Fatalf("skill frontmatter missing/misplaced at byte 0: %q", content[:min(len(content), 80)])
	}
	if !strings.Contains(string(content), "Start by running `lit quickstart`") {
		t.Fatalf("quickstart skill does not tell the agent to run `lit quickstart`")
	}
}

// validationReport is the part of `claude plugin validate --json`'s report this
// test judges. Manifest.Type is checked against the target, so a report whose
// shape moved decodes to a mismatch rather than to empty, passing finding
// lists. Contents stays raw: the plugin's manifests are all it validates today,
// and a report that starts listing contents must fail here until those
// findings are judged too, not decode as an empty, passing list.
type validationReport struct {
	Success  bool `json:"success"`
	Manifest struct {
		Type     string              `json:"type"`
		Errors   []validationFinding `json:"errors"`
		Warnings []validationFinding `json:"warnings"`
	} `json:"manifest"`
	Contents []json.RawMessage `json:"contents"`
}

type validationFinding struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// versionlessMessage is the validator's warning for a manifest that sets no
// version, the one warning the plugin is meant to draw.
const versionlessMessage = `No version specified. Consider adding a version following semver (e.g., "1.0.0")`

// TestClaudePluginManifestsPassOfficialValidation runs the marketplace and
// plugin manifests through the real `claude plugin validate`, the one checker
// that knows the manifest schema Claude Code actually loads — hand-modeling
// that schema in Go would drift the moment the CLI adds or retires a field.
// [LAW:verifiable-goals] testoracle.Path skips it where the CLI isn't installed
// and fails it in CI, which installs the pinned CLI
// (`.github/actions/install-claude`).
//
// It holds the manifests to strict validation, every warning a failure, but for
// one: the warning that the plugin sets no version. The plugin carries none on
// purpose, so that Claude Code versions it by commit and every merged change
// reaches installed users (RELEASING.md); --strict cannot be told that, so the
// report is read as JSON and that one warning, named by its exact path and
// message, is the only finding allowed. [LAW:one-source-of-truth]
func TestClaudePluginManifestsPassOfficialValidation(t *testing.T) {
	t.Parallel()
	claudeBin := testoracle.Path(t, "claude")
	root := mustRepoRoot(t)

	for _, tc := range []struct {
		target, manifestType string
		versionless          validationFinding
	}{
		{".", "marketplace", validationFinding{"plugins[0] plugin.json → version", versionlessMessage}},
		{"claude-plugin", "plugin", validationFinding{"version", versionlessMessage}},
	} {
		t.Run(tc.target, func(t *testing.T) {
			cmd := exec.Command(claudeBin, "plugin", "validate", tc.target, "--json")
			cmd.Dir = root
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("claude plugin validate %s --json failed: %v\nstdout:\n%s\nstderr:\n%s", tc.target, err, out, stderr.String())
			}
			var report validationReport
			if err := json.Unmarshal(out, &report); err != nil {
				t.Fatalf("claude plugin validate %s --json: report parse error = %v\n%s", tc.target, err, out)
			}
			if !report.Success || report.Manifest.Type != tc.manifestType || len(report.Manifest.Errors) != 0 || len(report.Contents) != 0 {
				t.Fatalf("claude plugin validate %s: want success on a %s manifest with no errors or contents findings\n%s", tc.target, tc.manifestType, out)
			}
			for _, w := range report.Manifest.Warnings {
				if w != tc.versionless {
					t.Errorf("claude plugin validate %s warns at %q: %s", tc.target, w.Path, w.Message)
				}
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
