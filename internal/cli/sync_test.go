package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
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

func TestBuildSyncPullPayloadNeverSyncedIsSkippedBranchMissing(t *testing.T) {
	// A branch the remote has never seen is the typed never_synced state, not a
	// parsed backend error string — the payload directs the caller to set the
	// upstream with a deterministic command.
	payload := buildSyncPullPayload("origin", "feature/local-only", store.SyncPullResult{State: store.SyncPullNeverSynced})
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

func TestBuildSyncPullPayloadProsePendingDirectsToReconcile(t *testing.T) {
	payload := buildSyncPullPayload("origin", "master", store.SyncPullResult{
		State:   store.SyncPullProsePending,
		Pending: make([]merge.ProsePending, 2),
	})
	if payload["status"] != "prose_pending" {
		t.Fatalf("status = %v, want prose_pending", payload["status"])
	}
	if payload["resolve_command"] != "lit sync reconcile" {
		t.Fatalf("resolve_command = %v, want `lit sync reconcile`", payload["resolve_command"])
	}
	if payload["pending"] != 2 {
		t.Fatalf("pending = %v, want 2", payload["pending"])
	}
}

func TestBuildSyncPullPayloadLinearizedIsOK(t *testing.T) {
	payload := buildSyncPullPayload("origin", "master", store.SyncPullResult{State: store.SyncPullLinearized})
	if payload["status"] != "ok" {
		t.Fatalf("status = %v, want ok", payload["status"])
	}
	if payload["state"] != "linearized" {
		t.Fatalf("state = %v, want linearized", payload["state"])
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

func TestPrintSyncPushPayloadRemoteEmptyAlwaysEmitsFirstPushMessage(t *testing.T) {
	payload := map[string]any{
		"status": "skipped",
		"reason": "remote_empty",
		"remote": "origin",
		"raw":    firstPushSkipMessage,
	}
	var out bytes.Buffer
	if err := printSyncPushPayload(&out, payload, false); err != nil {
		t.Fatalf("printSyncPushPayload() error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "first push") {
		t.Fatalf("printSyncPushPayload() = %q, want first-push message", got)
	}
	if !strings.Contains(got, "ONLY") {
		t.Fatalf("printSyncPushPayload() = %q, want emphasis that skip is only valid on first push", got)
	}
	if !strings.Contains(got, "do NOT ignore") && !strings.Contains(got, "something is wrong") {
		t.Fatalf("printSyncPushPayload() = %q, want warning that non-initial skips are a problem", got)
	}
}

func TestPrintSyncPullPayloadRemoteEmptyAlwaysEmitsFirstPushMessage(t *testing.T) {
	payload := map[string]any{
		"status": "skipped",
		"reason": "remote_empty",
		"remote": "origin",
		"raw":    firstPushSkipMessage,
	}
	var out bytes.Buffer
	if err := printSyncPullPayload(&out, payload, false); err != nil {
		t.Fatalf("printSyncPullPayload() error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "first push") {
		t.Fatalf("printSyncPullPayload() = %q, want first-push message", got)
	}
	if !strings.Contains(got, "ONLY") {
		t.Fatalf("printSyncPullPayload() = %q, want emphasis that skip is only valid on first push", got)
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
