package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// testIssuePrefix mints a PrefixSpec for fixtures through the same validating
// boundary production uses; fixtures cannot hold an un-normalized prefix.
func testIssuePrefix(t *testing.T, raw string) workspace.PrefixSpec {
	t.Helper()
	spec, err := workspace.ConfiguredPrefix(raw)
	if err != nil {
		t.Fatalf("ConfiguredPrefix(%q) error = %v", raw, err)
	}
	return spec
}

// initRepoForPrefixTest stamps a fresh git repo + lit init in a temp dir and
// returns the repo path. The returned cleanup restores cwd; tests run inside
// the repo for the duration of the test.
func initRepoForPrefixTest(t *testing.T) (repo string, runLit func(args ...string) (stdout string, err error)) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	repo = t.TempDir()
	runGit(t, repo, "init")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	var initBuf bytes.Buffer
	if err := Run(context.Background(), &initBuf, &initBuf, []string{"init", "--skip-hooks", "--skip-agents"}); err != nil {
		t.Fatalf("Run(init) error = %v\n%s", err, initBuf.String())
	}

	runLit = func(args ...string) (string, error) {
		var buf bytes.Buffer
		err := Run(context.Background(), &buf, &buf, args)
		return buf.String(), err
	}
	return repo, runLit
}

// prefixFromWorkspace extracts the issue_prefix value from `lit workspace` text
// output (one "key: value" line per field).
func prefixFromWorkspace(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "issue_prefix: "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// issueIDFromNew returns the issue id leading `lit new`'s issue-summary text
// row (the first whitespace-delimited token of the first non-empty line).
func issueIDFromNew(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

func TestPrefixSetPreviewDoesNotWriteConfig(t *testing.T) {
	repo, runLit := initRepoForPrefixTest(t)
	configPath := filepath.Join(repo, ".git", "links", "config.json")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}

	out, err := runLit("prefix", "set", "newproj")
	if err != nil {
		t.Fatalf("Run(prefix set preview) error = %v\n%s", err, out)
	}
	// Preview text: "issue_prefix: <prev> -> newproj (preview)"; never "(applied)".
	if !strings.Contains(out, "-> newproj (preview)") {
		t.Fatalf("preview output = %q, want a '-> newproj (preview)' line", out)
	}
	if strings.Contains(out, "(applied)") {
		t.Fatalf("preview reported applied; want preview only: %q", out)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config after preview) error = %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("config.json changed during preview; before=%q after=%q", string(before), string(after))
	}
}

func TestPrefixSetApplyWritesConfigAndOldIDsSurvive(t *testing.T) {
	repo, runLit := initRepoForPrefixTest(t)
	configPath := filepath.Join(repo, ".git", "links", "config.json")

	newOut, err := runLit("new", "--title", "first", "--topic", "onboarding", "--type", "task")
	if err != nil {
		t.Fatalf("Run(new) error = %v\n%s", err, newOut)
	}
	firstID := issueIDFromNew(newOut)
	if firstID == "" {
		t.Fatalf("first issue missing id: %s", newOut)
	}

	out, err := runLit("prefix", "set", "newproj", "--apply")
	if err != nil {
		t.Fatalf("Run(prefix set --apply) error = %v\n%s", err, out)
	}
	// Applied text: "issue_prefix: <prev> -> newproj (applied)".
	if !strings.Contains(out, "-> newproj (applied)") {
		t.Fatalf("apply output = %q, want a '-> newproj (applied)' line", out)
	}

	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config after apply) error = %v", err)
	}
	if !strings.Contains(string(configBytes), `"issue_prefix": "newproj"`) {
		t.Fatalf("config.json missing new prefix; got: %s", string(configBytes))
	}

	secondOut, err := runLit("new", "--title", "second", "--topic", "onboarding", "--type", "task")
	if err != nil {
		t.Fatalf("Run(new after rename) error = %v\n%s", err, secondOut)
	}
	secondID := issueIDFromNew(secondOut)
	if !strings.HasPrefix(secondID, "newproj-") {
		t.Fatalf("second issue ID = %q, want prefix newproj-", secondID)
	}

	showOut, err := runLit("show", firstID)
	if err != nil {
		t.Fatalf("Run(show old id after rename) error = %v\n%s", err, showOut)
	}
	if !strings.Contains(showOut, firstID) {
		t.Fatalf("show output missing old id %q: %s", firstID, showOut)
	}
}

func TestPrefixSetIdempotentWhenAlreadyCurrent(t *testing.T) {
	_, runLit := initRepoForPrefixTest(t)

	wsOut, err := runLit("workspace")
	if err != nil {
		t.Fatalf("Run(workspace) error = %v\n%s", err, wsOut)
	}
	current := prefixFromWorkspace(wsOut)
	if current == "" {
		t.Fatalf("workspace missing issue_prefix: %s", wsOut)
	}

	out, err := runLit("prefix", "set", current, "--apply")
	if err != nil {
		t.Fatalf("Run(prefix set same value) error = %v\n%s", err, out)
	}
	// Setting the current prefix is a no-op: "issue_prefix: <current> (prefix
	// unchanged)" — never an applied/preview transition.
	if !strings.Contains(out, "issue_prefix: "+current+" (prefix unchanged)") {
		t.Fatalf("idempotent output = %q, want '(prefix unchanged)' for %q", out, current)
	}
	if strings.Contains(out, "(applied)") {
		t.Fatalf("idempotent set reported applied; want unchanged: %q", out)
	}
}

func TestPrefixSetRejectsInvalidPrefix(t *testing.T) {
	_, runLit := initRepoForPrefixTest(t)

	out, err := runLit("prefix", "set", "ab", "--apply")
	if err == nil {
		t.Fatalf("expected error for too-short prefix; got output: %s", out)
	}
	if !strings.Contains(err.Error(), "invalid prefix") {
		t.Fatalf("error = %q, want it to mention invalid prefix", err.Error())
	}
}

func TestPrefixSetRejectsExtraPositionalArgs(t *testing.T) {
	_, runLit := initRepoForPrefixTest(t)

	out, err := runLit("prefix", "set", "newproj", "typo", "--apply")
	if err == nil {
		t.Fatalf("expected usage error for extra positional arg; got output: %s", out)
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("error = %q, want usage error mentioning extra args", err.Error())
	}
}
