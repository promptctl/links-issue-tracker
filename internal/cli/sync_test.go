package cli

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestMapRemotesByName(t *testing.T) {
	entries := []store.SyncRemote{
		{Name: "origin", URL: "https://fetch.example/repo.git"},
		{Name: "upstream", URL: "https://upstream.example/repo.git"},
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

func TestSameRemoteURLTreatsSCPLikeAndSSHFormsAsEqual(t *testing.T) {
	left := "git@github.com:brandon-fryslie/links-issue-tracker.git"
	right := "git+ssh://git@github.com/./brandon-fryslie/links-issue-tracker.git"
	if !sameRemoteURL(left, right) {
		t.Fatalf("sameRemoteURL(%q, %q) = false, want true", left, right)
	}
}

func TestSameRemoteURLDetectsDifferentRemotePaths(t *testing.T) {
	left := "git@github.com:brandon-fryslie/links-issue-tracker.git"
	right := "git+ssh://git@github.com/./brandon-fryslie/another.git"
	if sameRemoteURL(left, right) {
		t.Fatalf("sameRemoteURL(%q, %q) = true, want false", left, right)
	}
}

func TestSameRemoteURLSupportsBracketedIPv6SCPLikeHosts(t *testing.T) {
	left := "git@[fe80::1]:brandon-fryslie/links-issue-tracker.git"
	right := "ssh://git@[fe80::1]/brandon-fryslie/links-issue-tracker.git"
	if !sameRemoteURL(left, right) {
		t.Fatalf("sameRemoteURL(%q, %q) = false, want true", left, right)
	}
}

func TestSyncRemoteURLPrefixesGitHTTPSRemotes(t *testing.T) {
	got := syncRemoteURL("https://github.com/org/repo.git")
	want := "git+https://github.com/org/repo.git"
	if got != want {
		t.Fatalf("syncRemoteURL() = %q, want %q", got, want)
	}
}

func TestSyncRemoteURLPrefixesSCPLikeGitRemotes(t *testing.T) {
	got := syncRemoteURL("git@github.com:org/repo.git")
	want := "git+ssh://git@github.com/org/repo.git"
	if got != want {
		t.Fatalf("syncRemoteURL() = %q, want %q", got, want)
	}
}

func TestBuildSyncPullPayloadReturnsSkippedForMissingRemoteBranch(t *testing.T) {
	runErr := errors.New(`branch "feature/local-only" not found on remote`)
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
	if !strings.Contains(nextCommand, "lnks sync push --remote origin --set-upstream") {
		t.Fatalf("next_command missing deterministic remediation: %q", nextCommand)
	}
}

func TestBuildSyncPullPayloadReturnsErrorForNonMatchingFailure(t *testing.T) {
	runErr := errors.New("dolt pull origin master: fatal: network unavailable")
	_, err := buildSyncPullPayload("origin", "master", "", runErr)
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
		"branch": "master",
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
		"branch": "master",
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

func TestBuildRemoteSyncChanges(t *testing.T) {
	gitRemotes := []workspace.GitRemote{
		{Name: "origin", URL: "https://example.com/new-origin.git"},
		{Name: "upstream", URL: "https://example.com/upstream.git"},
	}
	doltRemotes := []store.SyncRemote{
		{Name: "origin", URL: "https://example.com/old-origin.git"},
		{Name: "fork", URL: "https://example.com/fork.git"},
	}

	got := buildRemoteSyncChanges(gitRemotes, doltRemotes)
	want := remoteSyncChanges{
		Added:   []string{"upstream"},
		Updated: []string{"origin"},
		Removed: []string{"fork"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildRemoteSyncChanges() = %#v, want %#v", got, want)
	}
}

func TestResolveSyncRemoteUsesRequestedRemoteFirst(t *testing.T) {
	remotes := []workspace.GitRemote{{Name: "origin"}, {Name: "upstream"}}
	got, err := resolveSyncRemote("origin", "upstream", remotes)
	if err != nil {
		t.Fatalf("resolveSyncRemote() error = %v", err)
	}
	if got != "origin" {
		t.Fatalf("resolveSyncRemote() = %q, want origin", got)
	}
}

func TestResolveSyncRemoteErrorsWhenRequestedRemoteIsUnknown(t *testing.T) {
	remotes := []workspace.GitRemote{{Name: "origin"}, {Name: "upstream"}}
	_, err := resolveSyncRemote("fork", "upstream", remotes)
	if err == nil {
		t.Fatal("resolveSyncRemote() error = nil, want error for unknown requested remote")
	}
}

func TestResolveSyncRemoteUsesUpstreamRemoteWhenPresent(t *testing.T) {
	remotes := []workspace.GitRemote{{Name: "origin"}, {Name: "upstream"}}
	got, err := resolveSyncRemote("", "upstream", remotes)
	if err != nil {
		t.Fatalf("resolveSyncRemote() error = %v", err)
	}
	if got != "upstream" {
		t.Fatalf("resolveSyncRemote() = %q, want upstream", got)
	}
}

func TestResolveSyncRemoteUsesSingleRemoteFallback(t *testing.T) {
	remotes := []workspace.GitRemote{{Name: "origin"}}
	got, err := resolveSyncRemote("", "", remotes)
	if err != nil {
		t.Fatalf("resolveSyncRemote() error = %v", err)
	}
	if got != "origin" {
		t.Fatalf("resolveSyncRemote() = %q, want origin", got)
	}
}

func TestResolveSyncRemoteIgnoresUnknownUpstreamRemote(t *testing.T) {
	remotes := []workspace.GitRemote{{Name: "origin"}, {Name: "upstream"}}
	got, err := resolveSyncRemote("", "missing", remotes)
	if err != nil {
		t.Fatalf("resolveSyncRemote() error = %v", err)
	}
	if got != "" {
		t.Fatalf("resolveSyncRemote() = %q, want empty", got)
	}
}

func TestResolveSyncRemoteReturnsEmptyWhenNoEligibleRemote(t *testing.T) {
	got, err := resolveSyncRemote("", "", nil)
	if err != nil {
		t.Fatalf("resolveSyncRemote() error = %v", err)
	}
	if got != "" {
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
