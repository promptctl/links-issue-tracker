package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestQuickstartRefreshRewritesManagedAssetsAndIsIdempotent(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}

	agentsPath := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("stale agents guidance\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(AGENTS.md) error = %v", err)
	}
	hookPath := filepath.Join(ws.GitCommonDir, "hooks", "pre-push")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(hooks dir) error = %v", err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/usr/bin/env bash\necho stale-hook\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(pre-push) error = %v", err)
	}

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	first := runQuickstartRefreshJSON(t)
	refresh := decodeQuickstartRefresh(t, first)
	if refresh["agents"].(map[string]any)["status"] != "updated" {
		t.Fatalf("agents refresh status = %v, want updated", refresh["agents"])
	}
	if refresh["hooks"].(map[string]any)["status"] != "updated" {
		t.Fatalf("hooks refresh status = %v, want updated", refresh["hooks"])
	}

	firstAgents, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("ReadFile(AGENTS.md) error = %v", err)
	}
	if string(firstAgents) == "stale agents guidance\n" {
		t.Fatal("quickstart --refresh should rewrite AGENTS.md")
	}

	second := runQuickstartRefreshJSON(t)
	secondRefresh := decodeQuickstartRefresh(t, second)
	if secondRefresh["agents"].(map[string]any)["status"] != "unchanged" {
		t.Fatalf("second agents refresh status = %v, want unchanged", secondRefresh["agents"])
	}
	if secondRefresh["hooks"].(map[string]any)["status"] != "unchanged" {
		t.Fatalf("second hooks refresh status = %v, want unchanged", secondRefresh["hooks"])
	}

	secondAgents, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("ReadFile(AGENTS.md second pass) error = %v", err)
	}
	if string(secondAgents) != string(firstAgents) {
		t.Fatal("quickstart --refresh should converge to a stable AGENTS.md rewrite")
	}
}

func TestQuickstartRefreshReportsIncompatibleHookAsSkipped(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}

	hookPath := filepath.Join(ws.GitCommonDir, "hooks", "pre-push")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(hooks dir) error = %v", err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/usr/bin/env sh\necho incompatible-hook\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(pre-push) error = %v", err)
	}

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	refresh := decodeQuickstartRefresh(t, runQuickstartRefreshJSON(t))
	hooks := refresh["hooks"].(map[string]any)
	if hooks["status"] != "skipped" {
		t.Fatalf("hooks refresh status = %v, want skipped", hooks)
	}
	if hooks["managed"] != false {
		t.Fatalf("hooks managed = %v, want false", hooks["managed"])
	}
	if hooks["reason"] != "incompatible" {
		t.Fatalf("hooks reason = %v, want incompatible", hooks["reason"])
	}
}

func runQuickstartRefreshJSON(t *testing.T) map[string]any {
	t.Helper()
	var stdout bytes.Buffer
	if err := Run(context.Background(), &stdout, &stdout, []string{"quickstart", "--refresh", "--json"}); err != nil {
		t.Fatalf("Run(quickstart --refresh --json) error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal(quickstart refresh output) error = %v", err)
	}
	return payload
}

func decodeQuickstartRefresh(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	refresh, ok := payload["refresh"].(map[string]any)
	if !ok {
		t.Fatalf("quickstart payload missing refresh report: %#v", payload)
	}
	return refresh
}
