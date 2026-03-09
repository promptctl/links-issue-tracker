package cli

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestMapRemotesByNamePrefersFetchScope(t *testing.T) {
	entries := []map[string]string{
		{"name": "origin", "url": "ssh://push.example/repo.git", "scope": "push"},
		{"name": "origin", "url": "https://fetch.example/repo.git", "scope": "fetch"},
		{"name": "upstream", "url": "https://upstream.example/repo.git"},
	}

	got := mapRemotesByName(entries)
	want := map[string]string{
		"origin":   "https://fetch.example/repo.git",
		"upstream": "https://upstream.example/repo.git",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapRemotesByName() = %#v, want %#v", got, want)
	}
}

func TestMapGitRemotesByName(t *testing.T) {
	remotes := []workspace.GitRemote{
		{Name: "origin", URL: "https://github.com/a/repo.git"},
		{Name: "upstream", URL: "https://github.com/b/repo.git"},
	}
	got := mapGitRemotesByName(remotes)
	want := map[string]string{
		"origin":   "https://github.com/a/repo.git",
		"upstream": "https://github.com/b/repo.git",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapGitRemotesByName() = %#v, want %#v", got, want)
	}
}

func TestSameRemoteURLIgnoresGitPrefix(t *testing.T) {
	if !sameRemoteURL("https://github.com/a/repo.git", "git+https://github.com/a/repo.git") {
		t.Fatal("expected URL comparison to ignore git+ prefix")
	}
}

func TestBuildSyncPullPayloadReturnsSkippedForMissingRemoteBranch(t *testing.T) {
	runErr := errors.New(`dolt pull origin feature/local-only: branch "feature/local-only" not found on remote`)
	payload, err := buildSyncPullPayload("origin", "feature/local-only", "", runErr)
	if err != nil {
		t.Fatalf("buildSyncPullPayload() error = %v", err)
	}
	if payload["status"] != "skipped" {
		t.Fatalf("status = %v, want skipped", payload["status"])
	}
	if payload["reason"] != "remote_branch_missing" {
		t.Fatalf("reason = %v, want remote_branch_missing", payload["reason"])
	}
	if payload["branch"] != "feature/local-only" {
		t.Fatalf("branch = %v, want feature/local-only", payload["branch"])
	}
	nextCommand := payload["next_command"].(string)
	if !strings.Contains(nextCommand, "lit sync push --remote origin --branch feature/local-only --set-upstream") {
		t.Fatalf("next_command missing deterministic remediation: %q", nextCommand)
	}
}

func TestBuildSyncPullPayloadReturnsErrorForNonMatchingFailure(t *testing.T) {
	runErr := errors.New("dolt pull origin main: fatal: network unavailable")
	_, err := buildSyncPullPayload("origin", "main", "", runErr)
	if err == nil {
		t.Fatal("expected error for non-matching pull failure")
	}
	if err.Error() != runErr.Error() {
		t.Fatalf("error = %v, want %v", err, runErr)
	}
}

func TestPrintSyncPullPayloadSkippedText(t *testing.T) {
	payload := map[string]any{
		"status":        "skipped",
		"remote":        "origin",
		"branch":        "feature/local-only",
		"next_command":  "lit sync push --remote origin --branch feature/local-only --set-upstream",
		"retry_command": "lit sync pull --remote origin --branch feature/local-only",
	}
	var out bytes.Buffer
	if err := printSyncPullPayload(&out, payload); err != nil {
		t.Fatalf("printSyncPullPayload() error = %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "skipped pull origin/feature/local-only: remote branch missing") {
		t.Fatalf("unexpected skipped text: %q", text)
	}
	if !strings.Contains(text, "lit sync push --remote origin --branch feature/local-only --set-upstream") {
		t.Fatalf("missing next command in text: %q", text)
	}
	if !strings.Contains(text, "lit sync pull --remote origin --branch feature/local-only") {
		t.Fatalf("missing retry command in text: %q", text)
	}
}
