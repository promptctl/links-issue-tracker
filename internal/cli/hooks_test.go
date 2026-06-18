package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func TestHooksInstallWritesPrePushHook(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runWsFamily(hooksFamily, context.Background(), &stdout, ws, []string{"install"}); err != nil {
		t.Fatalf("hooks install error = %v", err)
	}

	hookPath := filepath.Join(ws.GitCommonDir, "hooks", "pre-push")
	content, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("ReadFile(pre-push) error = %v", err)
	}
	text := string(content)
	if !strings.Contains(text, litHookBeginMarker) || !strings.Contains(text, litHookEndMarker) {
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
	if !strings.Contains(newHookText, litHookBeginMarker) || !strings.Contains(newHookText, litHookEndMarker) {
		t.Fatalf("new hook missing links managed section: %q", newHookText)
	}
}

func TestHooksInstallMigratesLegacyMarkers(t *testing.T) {
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
	hookPath := filepath.Join(hooksDir, "pre-push")
	seeded := "#!/usr/bin/env bash\necho user-prefix\n\n" +
		legacyHookBeginMarker + "\necho stale-managed-content\n" + legacyHookEndMarker + "\n"
	if err := os.WriteFile(hookPath, []byte(seeded), 0o755); err != nil {
		t.Fatalf("WriteFile(legacy pre-push) error = %v", err)
	}

	if err := runHooksInstall(new(bytes.Buffer), ws, nil); err != nil {
		t.Fatalf("runHooksInstall() error = %v", err)
	}

	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("ReadFile(pre-push) error = %v", err)
	}
	text := string(got)
	if strings.Contains(text, legacyHookBeginMarker) || strings.Contains(text, legacyHookEndMarker) {
		t.Fatalf("legacy markers not migrated: %q", text)
	}
	if strings.Count(text, litHookBeginMarker) != 1 || strings.Count(text, litHookEndMarker) != 1 {
		t.Fatalf("expected exactly one managed section, got: %q", text)
	}
	if !strings.Contains(text, "echo user-prefix") {
		t.Fatalf("user-owned prefix dropped: %q", text)
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
	if err := Run(context.Background(), &stdout, &stdout, []string{"hooks", "install"}); err != nil {
		t.Fatalf("Run(hooks install) error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "installed") {
		t.Fatalf("hooks install output = %q, want it to report installed", output)
	}
	if !strings.Contains(output, filepath.Join("hooks", "pre-push")) {
		t.Fatalf("hooks install output = %q, want the pre-push hook path", output)
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
