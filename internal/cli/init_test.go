package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// The embedded-default source path is pinned by TestInitHumanOutputShowsAgentsSource,
// which asserts the "(via embedded)" text the init surface now emits.

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

	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"init", "--skip-hooks"}); err != nil {
		t.Fatalf("Run(init --skip-hooks) error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "AGENTS.md (via global)") {
		t.Fatalf("init agents source = %q, want AGENTS.md (via global)", output)
	}
	if !strings.Contains(output, "CLAUDE.md (via global)") {
		t.Fatalf("init claude source = %q, want CLAUDE.md (via global)", output)
	}
}

func TestEnsureLinksAgentFilesMigratesLegacyMarkers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()

	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		seeded := "# user-owned heading\n\nUser content above.\n\n" +
			legacyAgentsMarkers.begin + "\nstale managed content\n" + legacyAgentsMarkers.end + "\n"
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
		if strings.Contains(text, legacyAgentsMarkers.begin) || strings.Contains(text, legacyAgentsMarkers.end) {
			t.Fatalf("%s: legacy markers not migrated: %q", name, text)
		}
		if strings.Count(text, litAgentsMarkers.begin) != 1 || strings.Count(text, litAgentsMarkers.end) != 1 {
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
	legacyBody := strings.ReplaceAll(section.text, litAgentsMarkers.begin, legacyAgentsMarkers.begin)
	legacyBody = strings.ReplaceAll(legacyBody, litAgentsMarkers.end, legacyAgentsMarkers.end)

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
		if strings.Contains(text, legacyAgentsMarkers.begin) || strings.Contains(text, legacyAgentsMarkers.end) {
			t.Fatalf("%s: legacy markers not migrated: %q", name, text)
		}
		if strings.Count(text, litAgentsMarkers.begin) != 1 || strings.Count(text, litAgentsMarkers.end) != 1 {
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
	if !strings.Contains(output, "`lit workflows`") || !strings.Contains(output, "`lit workflows edit <id-or-point>`") {
		t.Fatalf("init human output = %q, want a `lit workflows` guidance pointer", output)
	}
}

// An agents-section override authored as plain guidance text — no markers, the
// convention every other managed template follows — used to replace the managed
// region with unmarked text on its first run and re-append the whole section on
// every run after, growing AGENTS.md and CLAUDE.md without bound
// (links-templates-1bai).
func TestEnsureLinksAgentFilesMarkerlessOverrideConverges(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	writeProjectTemplateOverride(t, repo, templates.AgentsSectionTemplateName, "## House rules\n\nRun `lit quickstart` first.\n")

	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		seeded := "# user-owned heading\n\nUser content above.\n"
		if err := os.WriteFile(filepath.Join(repo, name), []byte(seeded), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}

	if _, _, err := ensureLinksAgentFiles(repo); err != nil {
		t.Fatalf("ensureLinksAgentFiles() first run error = %v", err)
	}
	first := map[string]string{
		"AGENTS.md": readFileString(t, filepath.Join(repo, "AGENTS.md")),
		"CLAUDE.md": readFileString(t, filepath.Join(repo, "CLAUDE.md")),
	}

	for run := 2; run <= 4; run++ {
		agentsResult, claudeResult, err := ensureLinksAgentFiles(repo)
		if err != nil {
			t.Fatalf("ensureLinksAgentFiles() run %d error = %v", run, err)
		}
		if agentsResult.Changed || claudeResult.Changed {
			t.Fatalf("run %d reported a change on already-converged files: AGENTS.md changed=%v, CLAUDE.md changed=%v",
				run, agentsResult.Changed, claudeResult.Changed)
		}
		for name, want := range first {
			if got := readFileString(t, filepath.Join(repo, name)); got != want {
				t.Fatalf("%s drifted on run %d:\ngot  %q\nwant %q", name, run, got, want)
			}
		}
	}

	for name, text := range first {
		if n := strings.Count(text, "## House rules"); n != 1 {
			t.Fatalf("%s: override body appears %d times, want 1: %q", name, n, text)
		}
		if strings.Count(text, litAgentsMarkers.begin) != 1 || strings.Count(text, litAgentsMarkers.end) != 1 {
			t.Fatalf("%s: expected exactly one managed section, got: %q", name, text)
		}
		if !strings.Contains(text, "# user-owned heading") || !strings.Contains(text, "User content above.") {
			t.Fatalf("%s: user content dropped: %q", name, text)
		}
	}
}

// A malformed override cannot converge whatever lit does with it, so `lit init`
// refuses it by name instead of writing a file that would grow on every run.
func TestInitRejectsUnbalancedAgentsOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	runGit(t, repo, "init")
	writeProjectTemplateOverride(t, repo, templates.AgentsSectionTemplateName,
		litAgentsMarkers.begin+"\nhouse rules\n")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	var stdout bytes.Buffer
	err = Run(context.Background(), &stdout, &stdout, []string{"init", "--skip-hooks"})
	if err == nil {
		t.Fatalf("Run(init) succeeded on an unbalanced override; output = %q", stdout.String())
	}
	if got := ExitCode(err); got != ExitValidation {
		t.Fatalf("ExitCode() = %d, want %d (%v)", got, ExitValidation, err)
	}
	if !strings.Contains(err.Error(), "agent section template (via project)") {
		t.Fatalf("error = %v, want it to name the template and its layer", err)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "AGENTS.md")); !os.IsNotExist(statErr) {
		t.Fatalf("AGENTS.md written despite the refusal: %v", statErr)
	}
}

func writeProjectTemplateOverride(t *testing.T, repo, name, content string) {
	t.Helper()
	path := filepath.Join(repo, ".lit", "templates", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return string(content)
}
