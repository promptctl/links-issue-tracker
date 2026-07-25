package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

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

// A held free-text conflict no longer renders as a benign stdout payload — it
// routes through the one sync-failure contract as a returned error, so `lit sync
// pull` exits ExitConflict like `lit sync reconcile` does for the identical state.
// syncFailureFromPull is the pure mapping the command uses; this pins that a
// prose-pending pull yields the proseHeld contract and every non-held state does not.
func TestSyncFailureFromPullHoldsProseConflict(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	failure, held := syncFailureFromPull("origin", "master", store.SyncPullResult{
		State:              store.SyncPullProsePending,
		Ahead:              2,
		Behind:             3,
		OldestDivergedUnix: now.Add(-3 * time.Hour).Unix(),
		Pending:            make([]merge.ProsePending, 2),
	}, now)
	if !held {
		t.Fatal("syncFailureFromPull did not classify a prose-pending pull as held")
	}
	// This test's job is the MAPPING (held, class, age). The pull→held→ExitConflict
	// exit contract is pinned end-to-end by TestExplicitPullSurfacesContractOnProseHeld
	// and at the type level by TestSyncFailureErrorExitAndRemediation, so it is not
	// re-asserted here.
	if failure.Failure.Class != syncFailureProseHeld {
		t.Fatalf("class = %q, want %q", failure.Failure.Class, syncFailureProseHeld)
	}
	if failure.Failure.Age != 3*time.Hour {
		t.Fatalf("age = %v, want 3h (derived from OldestDivergedUnix)", failure.Failure.Age)
	}
	// Every non-held state stays a printable payload, not a contract error.
	for _, state := range []store.SyncPullState{
		store.SyncPullUpToDate, store.SyncPullFastForwarded, store.SyncPullLinearized,
		store.SyncPullAhead, store.SyncPullNeverSynced,
	} {
		if _, held := syncFailureFromPull("origin", "master", store.SyncPullResult{State: state}, now); held {
			t.Fatalf("state %q wrongly classified as a held conflict", state)
		}
	}
}

func TestBuildSyncPullPayloadOKStates(t *testing.T) {
	for _, tc := range []struct {
		state store.SyncPullState
		want  string
	}{
		{store.SyncPullUpToDate, "up_to_date"},
		{store.SyncPullFastForwarded, "fast_forwarded"},
		{store.SyncPullLinearized, "linearized"},
		{store.SyncPullAhead, "ahead"},
	} {
		payload := buildSyncPullPayload("origin", "master", store.SyncPullResult{State: tc.state})
		if payload["status"] != "ok" {
			t.Fatalf("state %q: status = %v, want ok", tc.state, payload["status"])
		}
		if payload["state"] != tc.want {
			t.Fatalf("state %q: payload state = %v, want %q", tc.state, payload["state"], tc.want)
		}
	}
}

func TestBuildSyncPullPayloadUnknownStateSurfaces(t *testing.T) {
	// A SyncPullState the renderer does not enumerate must not masquerade as ok.
	payload := buildSyncPullPayload("origin", "master", store.SyncPullResult{State: store.SyncPullState("weird_new_state")})
	if payload["status"] != "unknown" {
		t.Fatalf("status = %v, want unknown (unmapped state must surface, not render ok)", payload["status"])
	}
	if payload["state"] != "weird_new_state" {
		t.Fatalf("state = %v, want weird_new_state", payload["state"])
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

func TestPrintSyncPullPayloadVerboseOKShowsState(t *testing.T) {
	for _, state := range []string{"up_to_date", "fast_forwarded", "linearized", "ahead"} {
		payload := map[string]any{"status": "ok", "state": state, "remote": "origin", "branch": "master"}
		var out bytes.Buffer
		if err := printSyncPullPayload(&out, payload, true); err != nil {
			t.Fatalf("printSyncPullPayload(%q) error = %v", state, err)
		}
		text := out.String()
		if !strings.Contains(text, "origin/master") {
			t.Fatalf("verbose ok text missing remote/branch for %q: %q", state, text)
		}
		if !strings.Contains(text, "("+state+")") {
			t.Fatalf("verbose ok text missing (%s) suffix: %q", state, text)
		}
	}
}

func TestPrintSyncPullPayloadUnknownStateAlwaysSurfaces(t *testing.T) {
	payload := map[string]any{"status": "unknown", "state": "weird_new_state", "remote": "origin", "branch": "master"}
	// Must surface even in non-verbose mode — a bug must never hide behind "pulled".
	var out bytes.Buffer
	if err := printSyncPullPayload(&out, payload, false); err != nil {
		t.Fatalf("printSyncPullPayload error = %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "weird_new_state") || !strings.Contains(text, "bug") {
		t.Fatalf("unknown-state text does not surface the state and flag it as a bug: %q", text)
	}
}

// printSyncPullPayload no longer renders a prose_pending case: a held conflict is
// a returned SyncFailureError (see TestSyncFailureFromPullHoldsProseConflict and
// the contract-shape tests in sync_failure_test.go), never a status payload. A
// prose_pending map reaching the printer would be a routing bug, so the printer's
// unknown-state guard surfaces it loudly rather than as a bland "pulled".
func TestPrintSyncPullPayloadRejectsStrayProsePending(t *testing.T) {
	payload := map[string]any{"status": "unknown", "state": "prose_pending", "remote": "origin", "branch": "master"}
	var out bytes.Buffer
	if err := printSyncPullPayload(&out, payload, false); err != nil {
		t.Fatalf("printSyncPullPayload() error = %v", err)
	}
	if text := out.String(); !strings.Contains(text, "prose_pending") || !strings.Contains(text, "bug") {
		t.Fatalf("stray prose_pending not surfaced as a bug: %q", text)
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
