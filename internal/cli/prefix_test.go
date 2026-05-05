package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if err := Run(context.Background(), &initBuf, &initBuf, []string{"init", "--skip-hooks", "--skip-agents", "--json"}); err != nil {
		t.Fatalf("Run(init) error = %v\n%s", err, initBuf.String())
	}

	runLit = func(args ...string) (string, error) {
		var buf bytes.Buffer
		err := Run(context.Background(), &buf, &buf, args)
		return buf.String(), err
	}
	return repo, runLit
}

func TestPrefixSetPreviewDoesNotWriteConfig(t *testing.T) {
	repo, runLit := initRepoForPrefixTest(t)
	configPath := filepath.Join(repo, ".git", "links", "config.json")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}

	out, err := runLit("prefix", "set", "newproj", "--json")
	if err != nil {
		t.Fatalf("Run(prefix set preview) error = %v\n%s", err, out)
	}
	var result prefixSetResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("json.Unmarshal(preview) error = %v\noutput = %q", err, out)
	}
	if result.Applied {
		t.Fatalf("preview reported Applied=true; want false")
	}
	if result.Current != "newproj" {
		t.Fatalf("preview Current = %q, want %q", result.Current, "newproj")
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

	newOut, err := runLit("new", "--title", "first", "--topic", "onboarding", "--type", "task", "--json")
	if err != nil {
		t.Fatalf("Run(new) error = %v\n%s", err, newOut)
	}
	var firstIssue map[string]any
	if err := json.Unmarshal([]byte(newOut), &firstIssue); err != nil {
		t.Fatalf("json.Unmarshal(new) error = %v\n%s", err, newOut)
	}
	firstID, _ := firstIssue["id"].(string)
	if firstID == "" {
		t.Fatalf("first issue missing id: %s", newOut)
	}

	out, err := runLit("prefix", "set", "newproj", "--apply", "--json")
	if err != nil {
		t.Fatalf("Run(prefix set --apply) error = %v\n%s", err, out)
	}
	var result prefixSetResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("json.Unmarshal(apply) error = %v\noutput = %q", err, out)
	}
	if !result.Applied {
		t.Fatalf("apply reported Applied=false; want true")
	}
	if result.Current != "newproj" {
		t.Fatalf("apply Current = %q, want %q", result.Current, "newproj")
	}

	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config after apply) error = %v", err)
	}
	if !strings.Contains(string(configBytes), `"issue_prefix": "newproj"`) {
		t.Fatalf("config.json missing new prefix; got: %s", string(configBytes))
	}

	secondOut, err := runLit("new", "--title", "second", "--topic", "onboarding", "--type", "task", "--json")
	if err != nil {
		t.Fatalf("Run(new after rename) error = %v\n%s", err, secondOut)
	}
	var secondIssue map[string]any
	if err := json.Unmarshal([]byte(secondOut), &secondIssue); err != nil {
		t.Fatalf("json.Unmarshal(new after rename) error = %v\n%s", err, secondOut)
	}
	secondID, _ := secondIssue["id"].(string)
	if !strings.HasPrefix(secondID, "newproj-") {
		t.Fatalf("second issue ID = %q, want prefix newproj-", secondID)
	}

	showOut, err := runLit("show", firstID, "--json")
	if err != nil {
		t.Fatalf("Run(show old id after rename) error = %v\n%s", err, showOut)
	}
	if !strings.Contains(showOut, firstID) {
		t.Fatalf("show output missing old id %q: %s", firstID, showOut)
	}
}

func TestPrefixSetIdempotentWhenAlreadyCurrent(t *testing.T) {
	_, runLit := initRepoForPrefixTest(t)

	wsOut, err := runLit("workspace", "--json")
	if err != nil {
		t.Fatalf("Run(workspace) error = %v\n%s", err, wsOut)
	}
	var ws map[string]string
	if err := json.Unmarshal([]byte(wsOut), &ws); err != nil {
		t.Fatalf("json.Unmarshal(workspace) error = %v\n%s", err, wsOut)
	}
	current := ws["issue_prefix"]
	if current == "" {
		t.Fatalf("workspace missing issue_prefix: %s", wsOut)
	}

	out, err := runLit("prefix", "set", current, "--apply", "--json")
	if err != nil {
		t.Fatalf("Run(prefix set same value) error = %v\n%s", err, out)
	}
	var result prefixSetResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("json.Unmarshal(idempotent) error = %v\n%s", err, out)
	}
	if result.Applied {
		t.Fatalf("idempotent set should report Applied=false; got true")
	}
	if result.Current != current {
		t.Fatalf("idempotent Current = %q, want %q", result.Current, current)
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
