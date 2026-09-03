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

func initInRepo(t *testing.T, repo string) string {
	t.Helper()
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
	return stdout.String()
}

func readNextSkill(t *testing.T, repo string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(nextSkillRelPath)))
	if err != nil {
		t.Fatalf("ReadFile(next skill) error = %v", err)
	}
	return string(got)
}

// [LAW:verifiable-goals] The epic's contract: a repository that runs `lit init`
// gets a working /next, shipped from the same binary whose commands it names.
func TestInitWritesNextSkillFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	runGit(t, repo, "init")

	output := initInRepo(t, repo)
	if !strings.Contains(output, "/next skill (via embedded)") {
		t.Fatalf("init human output = %q, want /next skill (via embedded)", output)
	}

	text := readNextSkill(t, repo)
	// The harness parses YAML frontmatter only at byte 0, so the managed
	// markers must sit inside the body rather than wrap the file.
	if !strings.HasPrefix(text, "---\nname: next\n") {
		t.Fatalf("SKILL.md must start with skill frontmatter, got: %q", text[:min(len(text), 80)])
	}
	if strings.Count(text, litAgentsBeginMarker) != 1 || strings.Count(text, litAgentsEndMarker) != 1 {
		t.Fatalf("expected exactly one managed section, got: %q", text)
	}
	embedded, err := templates.EmbeddedDefault(templates.NextSkillTemplateName)
	if err != nil {
		t.Fatalf("EmbeddedDefault() error = %v", err)
	}
	if !strings.Contains(text, string(embedded)) {
		t.Fatalf("managed section does not match the embedded template:\n%s", text)
	}
}

func TestInitNextSkillIdempotent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	runGit(t, repo, "init")

	initInRepo(t, repo)
	first := readNextSkill(t, repo)

	output := initInRepo(t, repo)
	if readNextSkill(t, repo) != first {
		t.Fatal("re-running init mutated SKILL.md")
	}
	if !strings.Contains(output, "Up to date:") || !strings.Contains(output, "/next skill") {
		t.Fatalf("re-init output = %q, want /next skill reported up to date", output)
	}
}

func TestInitNextSkillProjectOverrideWins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	runGit(t, repo, "init")

	projectTemplates := filepath.Join(repo, ".lit", "templates")
	if err := os.MkdirAll(projectTemplates, 0o755); err != nil {
		t.Fatalf("MkdirAll(project templates) error = %v", err)
	}
	override := litAgentsBeginMarker + "\nproject-specific /next procedure\n" + litAgentsEndMarker + "\n"
	if err := os.WriteFile(filepath.Join(projectTemplates, templates.NextSkillTemplateName), []byte(override), 0o644); err != nil {
		t.Fatalf("WriteFile(project override) error = %v", err)
	}

	output := initInRepo(t, repo)
	if !strings.Contains(output, "/next skill (via project)") {
		t.Fatalf("init human output = %q, want /next skill (via project)", output)
	}
	if !strings.Contains(readNextSkill(t, repo), "project-specific /next procedure") {
		t.Fatal("project override did not win over the embedded default")
	}
}

// An override authored as plain guidance text — the quickstart-template
// convention, no markers — must converge exactly like the marker-carrying
// default: one managed section, byte-idempotent across runs.
func TestEnsureNextSkillFileMarkerlessOverrideConverges(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()

	projectTemplates := filepath.Join(repo, ".lit", "templates")
	if err := os.MkdirAll(projectTemplates, 0o755); err != nil {
		t.Fatalf("MkdirAll(project templates) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectTemplates, templates.NextSkillTemplateName), []byte("plain override guidance\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(markerless override) error = %v", err)
	}

	if _, err := ensureNextSkillFile(repo); err != nil {
		t.Fatalf("ensureNextSkillFile() first run error = %v", err)
	}
	first := readNextSkill(t, repo)

	result, err := ensureNextSkillFile(repo)
	if err != nil {
		t.Fatalf("ensureNextSkillFile() second run error = %v", err)
	}
	if result.Changed {
		t.Fatal("second run with an unchanged markerless override reported a change")
	}
	if got := readNextSkill(t, repo); got != first {
		t.Fatalf("markerless override did not converge:\nfirst:  %q\nsecond: %q", first, got)
	}
	if strings.Count(first, "plain override guidance") != 1 || strings.Count(first, litAgentsBeginMarker) != 1 {
		t.Fatalf("expected exactly one managed section from the markerless override, got: %q", first)
	}
}

// A pre-existing hand-authored SKILL.md with no markers is adopted the way a
// marker-less AGENTS.md is: the user's content stays, the managed section is
// appended once, and subsequent runs reconcile in place.
func TestEnsureNextSkillFileAdoptsMarkerlessExistingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()

	skillPath := filepath.Join(repo, filepath.FromSlash(nextSkillRelPath))
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(skill dir) error = %v", err)
	}
	seeded := "---\nname: next\ndescription: Hand-authored skill\n---\n\nMy own procedure.\n"
	if err := os.WriteFile(skillPath, []byte(seeded), 0o644); err != nil {
		t.Fatalf("WriteFile(hand-authored skill) error = %v", err)
	}

	if _, err := ensureNextSkillFile(repo); err != nil {
		t.Fatalf("ensureNextSkillFile() adoption run error = %v", err)
	}
	first := readNextSkill(t, repo)
	if !strings.Contains(first, "My own procedure.") {
		t.Fatalf("hand-authored content dropped on adoption: %q", first)
	}
	if strings.Count(first, litAgentsBeginMarker) != 1 || strings.Count(first, litAgentsEndMarker) != 1 {
		t.Fatalf("expected exactly one appended managed section, got: %q", first)
	}

	result, err := ensureNextSkillFile(repo)
	if err != nil {
		t.Fatalf("ensureNextSkillFile() post-adoption run error = %v", err)
	}
	if result.Changed || readNextSkill(t, repo) != first {
		t.Fatal("adopted file did not converge on the second run")
	}
}

// lit owns only the marker-delimited body; the frontmatter and anything else
// the user adds around it survive a refresh.
func TestEnsureNextSkillFilePreservesUserContentOutsideMarkers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()

	skillPath := filepath.Join(repo, filepath.FromSlash(nextSkillRelPath))
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(skill dir) error = %v", err)
	}
	seeded := "---\nname: next\ndescription: My customized description\n---\n\n" +
		litAgentsBeginMarker + "\nstale managed body\n" + litAgentsEndMarker + "\n\nUser postscript below.\n"
	if err := os.WriteFile(skillPath, []byte(seeded), 0o644); err != nil {
		t.Fatalf("WriteFile(seeded skill) error = %v", err)
	}

	result, err := ensureNextSkillFile(repo)
	if err != nil {
		t.Fatalf("ensureNextSkillFile() error = %v", err)
	}
	if !result.Changed || result.Created {
		t.Fatalf("refresh of stale body: changed=%v created=%v, want changed, not created", result.Changed, result.Created)
	}

	text := readNextSkill(t, repo)
	if strings.Contains(text, "stale managed body") {
		t.Fatalf("stale managed body not refreshed: %q", text)
	}
	if !strings.Contains(text, "My customized description") || !strings.Contains(text, "User postscript below.") {
		t.Fatalf("user-owned content dropped: %q", text)
	}
}
