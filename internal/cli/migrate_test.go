package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/legacydolt"
	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestMigrateDryRunDoesNotModifyFiles(t *testing.T) {
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
	if err := Run(context.Background(), &stdout, &stdout, []string{"migrate", "--json"}); err != nil {
		t.Fatalf("Run(migrate --json) error = %v", err)
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

func TestMigrateApplyRewritesAgentsAndInstallsLitHook(t *testing.T) {
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
	if err := Run(context.Background(), &stdout, &stdout, []string{"migrate", "--apply", "--json"}); err != nil {
		t.Fatalf("Run(migrate --apply --json) error = %v", err)
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

func TestMigrateApplyImportsBeadsIssueData(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	runGit(t, repo, "init")

	sourceStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "source-links"), "workspace-source")
	if err != nil {
		t.Fatalf("store.Open(source) error = %v", err)
	}
	t.Cleanup(func() { _ = sourceStore.Close() })
	if err := sourceStore.EnsureIssuePrefix(ctx, "source"); err != nil {
		t.Fatalf("EnsureIssuePrefix(source) error = %v", err)
	}

	created, err := sourceStore.CreateIssue(ctx, store.CreateIssueInput{
		Title:       "Imported from beads",
		Description: "beads payload",
		Topic:       "legacy",
		IssueType:   "bug",
		Priority:    1,
		Assignee:    "bmf",
		Labels:      []string{"legacy"},
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := sourceStore.AddComment(ctx, store.AddCommentInput{
		IssueID:   created.ID,
		Body:      "migration comment",
		CreatedBy: "bmf",
	}); err != nil {
		t.Fatalf("AddComment() error = %v", err)
	}

	beadsPath := filepath.Join(repo, ".beads")
	exportSummary, err := legacydolt.Export(ctx, sourceStore, filepath.Join(beadsPath, "beads.db"))
	if err != nil {
		t.Fatalf("legacydolt.Export() error = %v", err)
	}
	if exportSummary.Issues != 1 || exportSummary.Comments != 1 {
		t.Fatalf("exportSummary = %#v", exportSummary)
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
	if err := Run(ctx, &stdout, &stdout, []string{"migrate", "--apply", "--json"}); err != nil {
		t.Fatalf("Run(migrate --apply --json) error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if payload["data_imported"] != true {
		t.Fatalf("data_imported = %v, want true", payload["data_imported"])
	}
	importSource, ok := payload["import_source"].(string)
	if !ok {
		t.Fatalf("import_source = %v, want string", payload["import_source"])
	}
	normalizePath := func(path string) string {
		return strings.TrimPrefix(filepath.Clean(path), "/private")
	}
	resolvedImportSource := normalizePath(importSource)
	resolvedBeadsPath := normalizePath(beadsPath)
	if resolvedImportSource != resolvedBeadsPath {
		t.Fatalf("import_source = %q, want %q", importSource, beadsPath)
	}
	if issuesCount, ok := payload["import_issues"].(float64); !ok || int(issuesCount) != 1 {
		t.Fatalf("import_issues = %v, want 1", payload["import_issues"])
	}
	if commentsCount, ok := payload["import_comments"].(float64); !ok || int(commentsCount) != 1 {
		t.Fatalf("import_comments = %v, want 1", payload["import_comments"])
	}

	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	destStore, err := store.Open(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("store.Open(dest) error = %v", err)
	}
	t.Cleanup(func() { _ = destStore.Close() })

	imported, err := destStore.GetIssueDetail(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if imported.Issue.Title != created.Title {
		t.Fatalf("imported title = %q, want %q", imported.Issue.Title, created.Title)
	}
	if len(imported.Comments) != 1 || imported.Comments[0].Body != "migration comment" {
		t.Fatalf("imported comments = %#v", imported.Comments)
	}
	if len(imported.Issue.Labels) != 1 || imported.Issue.Labels[0] != "legacy" {
		t.Fatalf("imported labels = %#v", imported.Issue.Labels)
	}
	if _, err := os.Stat(beadsPath); !os.IsNotExist(err) {
		t.Fatalf(".beads should be removed after migrate apply, stat error: %v", err)
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
