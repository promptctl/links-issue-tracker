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

func TestMigrateBeadsDryRunDoesNotModifyFiles(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	hookPath := filepath.Join(repo, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hookPath, []byte("#!/usr/bin/env bash\nbeads sync push\necho keep\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(pre-push) error = %v", err)
	}
	agentsPath := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("Use beads everywhere.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(AGENTS.md) error = %v", err)
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
	if err := Run(context.Background(), &stdout, &stdout, []string{"migrate", "beads", "--json"}); err != nil {
		t.Fatalf("Run(migrate beads --json) error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if payload["mode"] != "dry-run" {
		t.Fatalf("mode = %v, want dry-run", payload["mode"])
	}

	hookContent, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("ReadFile(pre-push) error = %v", err)
	}
	if !strings.Contains(string(hookContent), "beads") {
		t.Fatalf("dry-run unexpectedly changed hook: %q", string(hookContent))
	}
	agentsContent, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("ReadFile(AGENTS.md) error = %v", err)
	}
	if !strings.Contains(string(agentsContent), "beads") {
		t.Fatalf("dry-run unexpectedly changed AGENTS.md: %q", string(agentsContent))
	}
}

func TestMigrateBeadsApplyRewritesAgentsAndInstallsLitHook(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	hookPath := filepath.Join(repo, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hookPath, []byte("#!/usr/bin/env bash\nbeads sync push\necho keep\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(pre-push) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.beads) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".beads", "state.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(.beads/state.json) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.claude) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), []byte(`{"hooks":{"SessionStart":[{"hooks":[{"command":"beads sync"}]}]}}`), 0o644); err != nil {
		t.Fatalf("WriteFile(.claude/settings.json) error = %v", err)
	}
	agentsPath := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("Use Beads and beads.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(AGENTS.md) error = %v", err)
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
	if err := Run(context.Background(), &stdout, &stdout, []string{"migrate", "beads", "--apply", "--json"}); err != nil {
		t.Fatalf("Run(migrate beads --apply --json) error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if payload["mode"] != "apply" {
		t.Fatalf("mode = %v, want apply", payload["mode"])
	}
	if payload["lit_hook_installed"] != true {
		t.Fatalf("lit_hook_installed = %v, want true", payload["lit_hook_installed"])
	}
	backupPath, ok := payload["backup_path"].(string)
	if !ok || strings.TrimSpace(backupPath) == "" {
		t.Fatalf("backup_path missing from payload: %#v", payload)
	}
	if _, err := os.Stat(filepath.Join(backupPath, "manifest.json")); err != nil {
		t.Fatalf("backup manifest missing: %v", err)
	}

	newHookContent, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("ReadFile(pre-push) error = %v", err)
	}
	newHook := string(newHookContent)
	if !strings.Contains(newHook, linksHookBeginMarker) || !strings.Contains(newHook, linksHookEndMarker) {
		t.Fatalf("expected lit managed hook markers, got: %q", newHook)
	}
	if !strings.Contains(newHook, "echo keep") {
		t.Fatalf("new hook should preserve existing user logic: %q", newHook)
	}
	if strings.Contains(strings.ToLower(newHook), "beads") {
		t.Fatalf("new hook still contains beads: %q", newHook)
	}
	agentsContent, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("ReadFile(AGENTS.md) error = %v", err)
	}
	if strings.Contains(strings.ToLower(string(agentsContent)), "beads") {
		t.Fatalf("AGENTS.md still contains beads: %q", string(agentsContent))
	}
	if !strings.Contains(string(agentsContent), "lnks") {
		t.Fatalf("AGENTS.md missing lnks replacement: %q", string(agentsContent))
	}
	if _, err := os.Stat(filepath.Join(repo, ".beads")); !os.IsNotExist(err) {
		t.Fatalf(".beads should be removed, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf(".claude/settings.json should be removed when only beads residue remains, stat error: %v", err)
	}
}

func TestBuildConfigCleanupPlanPreservesJSONWithoutBeadsMutations(t *testing.T) {
	t.Parallel()

	plan, err := buildConfigCleanupPlan("/tmp/settings.json", []byte(`{"hooks":{"SessionStart":[{"hooks":[{"command":"sync push"}]}]}}`))
	if err != nil {
		t.Fatalf("buildConfigCleanupPlan() error = %v", err)
	}
	if plan.Changed {
		t.Fatalf("plan.Changed = true, want false")
	}
	if plan.RemoveFile {
		t.Fatalf("plan.RemoveFile = true, want false")
	}
	if len(plan.NewContent) != 0 {
		t.Fatalf("plan.NewContent = %q, want empty", string(plan.NewContent))
	}
}
