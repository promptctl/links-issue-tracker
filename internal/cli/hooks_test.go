package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestHooksInstallWritesPrePushHook(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runHooks(&stdout, ws, []string{"install"}); err != nil {
		t.Fatalf("runHooks(install) error = %v", err)
	}

	hookPath := filepath.Join(ws.GitCommonDir, "hooks", "pre-push")
	content, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("ReadFile(pre-push) error = %v", err)
	}
	text := string(content)
	if !strings.Contains(text, linksHookBeginMarker) || !strings.Contains(text, linksHookEndMarker) {
		t.Fatalf("hook missing managed section markers: %q", text)
	}
	if !strings.Contains(text, "hook-triggered lit sync push failed") {
		t.Fatalf("hook missing warning output: %q", text)
	}
	if !strings.Contains(text, "LNKS_AUTOMATION_TRIGGER=\"git-pre-push\"") {
		t.Fatalf("hook missing automation trigger env: %q", text)
	}
	if !strings.Contains(text, "LNKS_AUTOMATION_TRACE_REF_FILE") {
		t.Fatalf("hook missing trace ref env: %q", text)
	}
	if !strings.Contains(text, "exit 0") {
		t.Fatalf("hook must never block push: %q", text)
	}
}

func TestHooksInstallPreservesExistingPrePushHook(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	hooksDir := filepath.Join(ws.GitCommonDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(hooks) error = %v", err)
	}
	original := "#!/usr/bin/env bash\necho custom-pre-push\n"
	originalPath := filepath.Join(hooksDir, "pre-push")
	if err := os.WriteFile(originalPath, []byte(original), 0o755); err != nil {
		t.Fatalf("WriteFile(pre-push) error = %v", err)
	}

	if err := runHooksInstall(new(bytes.Buffer), ws, nil); err != nil {
		t.Fatalf("runHooksInstall() error = %v", err)
	}

	newHook, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatalf("ReadFile(new pre-push) error = %v", err)
	}
	newHookText := string(newHook)
	if !strings.Contains(newHookText, "echo custom-pre-push") {
		t.Fatalf("new hook does not preserve existing logic: %q", newHookText)
	}
	if !strings.Contains(newHookText, linksHookBeginMarker) || !strings.Contains(newHookText, linksHookEndMarker) {
		t.Fatalf("new hook missing links managed section: %q", newHookText)
	}
}

func TestRunHooksViaCLI(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})

	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"hooks", "install", "--json"}); err != nil {
		t.Fatalf("Run(hooks install --json) error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal(hooks install output) error = %v", err)
	}
	if payload["status"] != "installed" {
		t.Fatalf("status = %v, want installed", payload["status"])
	}
	if strings.TrimSpace(payload["traces_dir"].(string)) == "" {
		t.Fatalf("traces_dir = %v, want non-empty", payload["traces_dir"])
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}
