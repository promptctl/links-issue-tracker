package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

func TestInitReportsAgentsSourceFromEmbeddedDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	runGit(t, repo, "init")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"init", "--skip-hooks", "--json"}); err != nil {
		t.Fatalf("Run(init --skip-hooks --json) error = %v", err)
	}

	var report initReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal(init output) error = %v\noutput = %q", err, stdout.String())
	}
	if report.AgentsSource != string(templates.SourceEmbedded) {
		t.Fatalf("init agents_source = %q, want %q", report.AgentsSource, templates.SourceEmbedded)
	}
	if report.ClaudeSource != string(templates.SourceEmbedded) {
		t.Fatalf("init claude_source = %q, want %q", report.ClaudeSource, templates.SourceEmbedded)
	}
}

func TestInitReportsAgentsSourceFromGlobalOverride(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	repo := t.TempDir()
	runGit(t, repo, "init")

	embedded, err := templates.EmbeddedDefault(templates.AgentsSectionTemplateName)
	if err != nil {
		t.Fatalf("EmbeddedDefault() error = %v", err)
	}
	globalPath := filepath.Join(xdg, "links-issue-tracker", "templates", templates.AgentsSectionTemplateName)
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(global templates) error = %v", err)
	}
	if err := os.WriteFile(globalPath, embedded, 0o644); err != nil {
		t.Fatalf("WriteFile(global override) error = %v", err)
	}

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	var jsonBuf bytes.Buffer
	if err := Run(context.Background(), &jsonBuf, &jsonBuf, []string{"init", "--skip-hooks", "--json"}); err != nil {
		t.Fatalf("Run(init --skip-hooks --json) error = %v", err)
	}
	var report initReport
	if err := json.Unmarshal(jsonBuf.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal(init output) error = %v\noutput = %q", err, jsonBuf.String())
	}
	if report.AgentsSource != string(templates.SourceGlobal) {
		t.Fatalf("init agents_source = %q, want %q", report.AgentsSource, templates.SourceGlobal)
	}
	if report.ClaudeSource != string(templates.SourceGlobal) {
		t.Fatalf("init claude_source = %q, want %q", report.ClaudeSource, templates.SourceGlobal)
	}
}

func TestEnsureLinksAgentFilesMigratesLegacyMarkers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()

	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		seeded := "# user-owned heading\n\nUser content above.\n\n" +
			legacyAgentsBeginMarker + "\nstale managed content\n" + legacyAgentsEndMarker + "\n"
		if err := os.WriteFile(filepath.Join(repo, name), []byte(seeded), 0o644); err != nil {
			t.Fatalf("WriteFile(%s legacy) error = %v", name, err)
		}
	}

	if _, _, err := ensureLinksAgentFiles(repo); err != nil {
		t.Fatalf("ensureLinksAgentFiles() error = %v", err)
	}

	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		got, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", name, err)
		}
		text := string(got)
		if strings.Contains(text, legacyAgentsBeginMarker) || strings.Contains(text, legacyAgentsEndMarker) {
			t.Fatalf("%s: legacy markers not migrated: %q", name, text)
		}
		if strings.Count(text, litAgentsBeginMarker) != 1 || strings.Count(text, litAgentsEndMarker) != 1 {
			t.Fatalf("%s: expected exactly one managed section, got: %q", name, text)
		}
		if !strings.Contains(text, "# user-owned heading") || !strings.Contains(text, "User content above.") {
			t.Fatalf("%s: user content dropped: %q", name, text)
		}
	}
}

// Regression: when only the markers are legacy and the managed body already
// matches the current template byte-for-byte, the migration must still persist
// and be reported as changed. The earlier change signal compared against the
// post-migration content, so a marker-only diff was silently dropped.
func TestEnsureLinksAgentFilesMigratesMarkerOnlyDifference(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()

	section, _, err := renderLinksAgentsSection(repo)
	if err != nil {
		t.Fatalf("renderLinksAgentsSection() error = %v", err)
	}
	legacyBody := strings.ReplaceAll(section, litAgentsBeginMarker, legacyAgentsBeginMarker)
	legacyBody = strings.ReplaceAll(legacyBody, litAgentsEndMarker, legacyAgentsEndMarker)

	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		seeded := "# user-owned heading\n\nUser content above.\n\n" + legacyBody
		if err := os.WriteFile(filepath.Join(repo, name), []byte(seeded), 0o644); err != nil {
			t.Fatalf("WriteFile(%s legacy) error = %v", name, err)
		}
	}

	agentsResult, claudeResult, err := ensureLinksAgentFiles(repo)
	if err != nil {
		t.Fatalf("ensureLinksAgentFiles() error = %v", err)
	}
	if !agentsResult.Changed || !claudeResult.Changed {
		t.Fatalf("marker-only migration not reported as changed: AGENTS.md changed=%v, CLAUDE.md changed=%v",
			agentsResult.Changed, claudeResult.Changed)
	}

	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		got, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", name, err)
		}
		text := string(got)
		if strings.Contains(text, legacyAgentsBeginMarker) || strings.Contains(text, legacyAgentsEndMarker) {
			t.Fatalf("%s: legacy markers not migrated: %q", name, text)
		}
		if strings.Count(text, litAgentsBeginMarker) != 1 || strings.Count(text, litAgentsEndMarker) != 1 {
			t.Fatalf("%s: expected exactly one managed section, got: %q", name, text)
		}
		if !strings.Contains(text, "# user-owned heading") || !strings.Contains(text, "User content above.") {
			t.Fatalf("%s: user content dropped: %q", name, text)
		}
	}
}

func TestInitHumanOutputShowsAgentsSource(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	runGit(t, repo, "init")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"init", "--skip-hooks"}); err != nil {
		t.Fatalf("Run(init --skip-hooks) error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "AGENTS.md (via embedded)") {
		t.Fatalf("init human output = %q, want AGENTS.md (via embedded)", output)
	}
	if !strings.Contains(output, "CLAUDE.md (via embedded)") {
		t.Fatalf("init human output = %q, want CLAUDE.md (via embedded)", output)
	}
}
