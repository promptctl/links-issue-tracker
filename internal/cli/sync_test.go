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
	if !strings.Contains(nextCommand, "lit sync push --remote origin --set-upstream") {
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
		"next_command":  "lit sync push --remote origin --set-upstream",
		"retry_command": "lit sync pull --remote origin",
	}
	var out bytes.Buffer
	if err := printSyncPullPayload(&out, payload, true); err != nil {
		t.Fatalf("printSyncPullPayload() error = %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "skipped pull origin/feature/local-only: remote branch missing") {
		t.Fatalf("unexpected skipped text: %q", text)
	}
	if !strings.Contains(text, "lit sync push --remote origin --set-upstream") {
		t.Fatalf("missing next command in text: %q", text)
	}
	if !strings.Contains(text, "lit sync pull --remote origin") {
		t.Fatalf("missing retry command in text: %q", text)
	}
}

func TestPrintSyncPullPayloadSkippedTextWithoutVerboseOmitsRemoteDetails(t *testing.T) {
	payload := map[string]any{
		"status":        "skipped",
		"remote":        "origin",
		"branch":        "feature/local-only",
		"next_command":  "lit sync push --remote origin --set-upstream",
		"retry_command": "lit sync pull --remote origin",
	}
	var out bytes.Buffer
	if err := printSyncPullPayload(&out, payload, false); err != nil {
		t.Fatalf("printSyncPullPayload() error = %v", err)
	}
	text := out.String()
	if strings.Contains(text, "origin/feature/local-only") {
		t.Fatalf("printSyncPullPayload() unexpectedly includes remote details: %q", text)
	}
	if !strings.Contains(text, "sync pull skipped; run") {
		t.Fatalf("printSyncPullPayload() missing terse skipped guidance: %q", text)
	}
}

func TestPrintSyncPullPayloadNoRemoteSkippedText(t *testing.T) {
	payload := map[string]any{
		"status": "skipped",
		"reason": "no_sync_remote",
	}
	var out bytes.Buffer
	if err := printSyncPullPayload(&out, payload, false); err != nil {
		t.Fatalf("printSyncPullPayload() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "" {
		t.Fatalf("printSyncPullPayload() = %q, want empty output", got)
	}
}

func TestPrintSyncPullPayloadNoRemoteSkippedVerboseText(t *testing.T) {
	payload := map[string]any{
		"status": "skipped",
		"reason": "no_sync_remote",
	}
	var out bytes.Buffer
	if err := printSyncPullPayload(&out, payload, true); err != nil {
		t.Fatalf("printSyncPullPayload() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "skipped sync pull: no eligible git remote" {
		t.Fatalf("printSyncPullPayload() = %q, want verbose no-remote message", got)
	}
}

func TestPrintSyncPushPayloadNoRemoteSkippedText(t *testing.T) {
	payload := map[string]any{
		"status": "skipped",
		"reason": "no_sync_remote",
	}
	var out bytes.Buffer
	if err := printSyncPushPayload(&out, payload, false); err != nil {
		t.Fatalf("printSyncPushPayload() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "" {
		t.Fatalf("printSyncPushPayload() = %q, want empty output", got)
	}
}

func TestPrintSyncPushPayloadNoRemoteSkippedVerboseText(t *testing.T) {
	payload := map[string]any{
		"status": "skipped",
		"reason": "no_sync_remote",
	}
	var out bytes.Buffer
	if err := printSyncPushPayload(&out, payload, true); err != nil {
		t.Fatalf("printSyncPushPayload() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "skipped sync push: no eligible git remote" {
		t.Fatalf("printSyncPushPayload() = %q, want verbose no-remote message", got)
	}
}

func TestPrintSyncPullPayloadDefaultSuccessTextHidesRemoteDetails(t *testing.T) {
	payload := map[string]any{
		"status": "ok",
		"remote": "origin",
		"branch": "main",
		"raw":    "From origin",
	}
	var out bytes.Buffer
	if err := printSyncPullPayload(&out, payload, false); err != nil {
		t.Fatalf("printSyncPullPayload() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "pulled" {
		t.Fatalf("printSyncPullPayload() = %q, want pulled", got)
	}
}

func TestPrintSyncPushPayloadDefaultSuccessTextHidesRemoteDetails(t *testing.T) {
	payload := map[string]any{
		"status": "ok",
		"remote": "origin",
		"branch": "main",
		"raw":    "Pushing to origin",
	}
	var out bytes.Buffer
	if err := printSyncPushPayload(&out, payload, false); err != nil {
		t.Fatalf("printSyncPushPayload() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "pushed" {
		t.Fatalf("printSyncPushPayload() = %q, want pushed", got)
	}
}

func TestBuildSyncPushCommandArgsWithoutSyncBranchUsesDefaultPush(t *testing.T) {
	got := buildSyncPushCommandArgs("origin", "", false, false)
	want := []string{"push", "origin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildSyncPushCommandArgs() = %#v, want %#v", got, want)
	}
}

func TestBuildSyncPushCommandArgsBuildsHeadToSyncBranchRefspec(t *testing.T) {
	got := buildSyncPushCommandArgs("origin", "main", true, false)
	want := []string{"push", "-u", "origin", "HEAD:main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildSyncPushCommandArgs() = %#v, want %#v", got, want)
	}
}

func TestBuildSyncPushCommandArgsForceUsesSyncBranchRefspec(t *testing.T) {
	got := buildSyncPushCommandArgs("origin", "feature/local-only", false, true)
	want := []string{"push", "--force", "origin", "HEAD:feature/local-only"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildSyncPushCommandArgs() = %#v, want %#v", got, want)
	}
}

func TestBuildSyncPullCommandArgsWithoutBranchUsesDefaultPull(t *testing.T) {
	got := buildSyncPullCommandArgs("origin", "")
	want := []string{"pull", "origin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildSyncPullCommandArgs() = %#v, want %#v", got, want)
	}
}

func TestBuildSyncPullCommandArgsWithBranchUsesExplicitBranch(t *testing.T) {
	got := buildSyncPullCommandArgs("origin", "main")
	want := []string{"pull", "origin", "main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildSyncPullCommandArgs() = %#v, want %#v", got, want)
	}
}

func TestFirstNonEmptySyncBranchFollowsDeterministicPriority(t *testing.T) {
	got := firstNonEmptySyncBranch("debug", "default")
	if got != "debug" {
		t.Fatalf("firstNonEmptySyncBranch() = %q, want debug", got)
	}
	got = firstNonEmptySyncBranch("", "default")
	if got != "default" {
		t.Fatalf("firstNonEmptySyncBranch() = %q, want default", got)
	}
	got = firstNonEmptySyncBranch("", "")
	if got != "" {
		t.Fatalf("firstNonEmptySyncBranch() = %q, want empty", got)
	}
}

func TestResolveSyncRemoteUsesRequestedRemoteFirst(t *testing.T) {
	remotes := []workspace.GitRemote{{Name: "origin"}, {Name: "upstream"}}
	got := resolveSyncRemote("origin", "upstream", remotes)
	if got != "origin" {
		t.Fatalf("resolveSyncRemote() = %q, want origin", got)
	}
}

func TestResolveSyncRemoteUsesUpstreamRemoteWhenPresent(t *testing.T) {
	remotes := []workspace.GitRemote{{Name: "origin"}, {Name: "upstream"}}
	got := resolveSyncRemote("", "upstream", remotes)
	if got != "upstream" {
		t.Fatalf("resolveSyncRemote() = %q, want upstream", got)
	}
}

func TestResolveSyncRemoteUsesSingleRemoteFallback(t *testing.T) {
	remotes := []workspace.GitRemote{{Name: "origin"}}
	got := resolveSyncRemote("", "", remotes)
	if got != "origin" {
		t.Fatalf("resolveSyncRemote() = %q, want origin", got)
	}
}

func TestResolveSyncRemoteIgnoresUnknownUpstreamRemote(t *testing.T) {
	remotes := []workspace.GitRemote{{Name: "origin"}, {Name: "upstream"}}
	got := resolveSyncRemote("", "missing", remotes)
	if got != "" {
		t.Fatalf("resolveSyncRemote() = %q, want empty", got)
	}
}

func TestResolveSyncRemoteReturnsEmptyWhenNoEligibleRemote(t *testing.T) {
	if got := resolveSyncRemote("", "", nil); got != "" {
		t.Fatalf("resolveSyncRemote() = %q, want empty", got)
	}
}

func TestResolveSyncBranchUsesDebugOverrideWhenPresent(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "debug-branch")
	got, err := resolveSyncBranch(t.TempDir(), "origin")
	if err != nil {
		t.Fatalf("resolveSyncBranch() error = %v", err)
	}
	if got != "debug-branch" {
		t.Fatalf("resolveSyncBranch() = %q, want debug-branch", got)
	}
}

func TestResolveSyncBranchErrorsWhenDefaultBranchUnavailable(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "")
	_, err := resolveSyncBranch(t.TempDir(), "origin")
	if err == nil {
		t.Fatal("expected error when default branch is unavailable")
	}
	if !strings.Contains(err.Error(), debugSyncBranchEnvVar) {
		t.Fatalf("error = %q, want mention of %s", err.Error(), debugSyncBranchEnvVar)
	}
}
