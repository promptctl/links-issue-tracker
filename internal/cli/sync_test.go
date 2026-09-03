package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func TestMapRemotesByName(t *testing.T) {
	t.Parallel()
	entries := []storage.SyncRemote{
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
	t.Parallel()
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

func TestPrintSyncPullOutcomeNeverSyncedDirectsUpstreamSetup(t *testing.T) {
	t.Parallel()
	// A branch the remote has never seen is the typed never_synced state, not a
	// parsed backend error string — the printer directs the caller to set the
	// upstream with a deterministic command derived from the resolved remote.
	outcome := syncPullOutcome{remote: "origin", branch: "feature/local-only", state: storage.SyncPullNeverSynced}
	var out bytes.Buffer
	if err := printSyncPullOutcome(&out, outcome, true); err != nil {
		t.Fatalf("printSyncPullOutcome() error = %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "skipped pull origin/feature/local-only: remote branch missing") {
		t.Fatalf("unexpected skipped text: %q", text)
	}
	if !strings.Contains(text, "lit sync push --remote origin --set-upstream") {
		t.Fatalf("missing deterministic next command: %q", text)
	}
	if !strings.Contains(text, "lit sync pull --remote origin") {
		t.Fatalf("missing retry command: %q", text)
	}
}

// A held free-text conflict no longer renders as a benign stdout payload — it
// routes through the one sync-failure contract as a returned error, so `lit sync
// pull` exits ExitConflict like `lit sync reconcile` does for the identical state.
// syncFailureFromPull is the pure mapping the command uses; this pins that a
// prose-pending pull yields the proseHeld contract and every non-held state does not.
func TestSyncFailureFromPullHoldsProseConflict(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	failure, held := syncFailureFromPull("origin", "master", storage.SyncPullResult{
		State:              storage.SyncPullProsePending,
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
	if failure.Failure.BuildNote == "" {
		t.Fatal("syncFailureFromPull did not resolve BuildNote")
	}
	// Every non-held state stays a printable payload, not a contract error.
	for _, state := range []storage.SyncPullState{
		storage.SyncPullUpToDate, storage.SyncPullFastForwarded, storage.SyncPullLinearized,
		storage.SyncPullAhead, storage.SyncPullNeverSynced,
	} {
		if _, held := syncFailureFromPull("origin", "master", storage.SyncPullResult{State: state}, now); held {
			t.Fatalf("state %q wrongly classified as a held conflict", state)
		}
	}
}

func TestPrintSyncPullOutcomeNeverSyncedWithoutVerboseOmitsRemoteDetails(t *testing.T) {
	t.Parallel()
	outcome := syncPullOutcome{remote: "origin", branch: "feature/local-only", state: storage.SyncPullNeverSynced}
	var out bytes.Buffer
	if err := printSyncPullOutcome(&out, outcome, false); err != nil {
		t.Fatalf("printSyncPullOutcome() error = %v", err)
	}
	text := out.String()
	if strings.Contains(text, "origin/feature/local-only") {
		t.Fatalf("printSyncPullOutcome() unexpectedly includes remote details: %q", text)
	}
	if !strings.Contains(text, "sync pull skipped; run") {
		t.Fatalf("printSyncPullOutcome() missing terse skipped guidance: %q", text)
	}
}

func TestPrintSyncPullOutcomeVerboseOKShowsState(t *testing.T) {
	t.Parallel()
	for _, state := range []storage.SyncPullState{
		storage.SyncPullUpToDate, storage.SyncPullFastForwarded, storage.SyncPullLinearized, storage.SyncPullAhead,
	} {
		outcome := syncPullOutcome{remote: "origin", branch: "master", state: state}
		var out bytes.Buffer
		if err := printSyncPullOutcome(&out, outcome, true); err != nil {
			t.Fatalf("printSyncPullOutcome(%q) error = %v", state, err)
		}
		text := out.String()
		if !strings.Contains(text, "origin/master") {
			t.Fatalf("verbose ok text missing remote/branch for %q: %q", state, text)
		}
		if !strings.Contains(text, "("+string(state)+")") {
			t.Fatalf("verbose ok text missing (%s) suffix: %q", state, text)
		}
	}
}

func TestPrintSyncPullOutcomeUnknownStateAlwaysSurfaces(t *testing.T) {
	t.Parallel()
	outcome := syncPullOutcome{remote: "origin", branch: "master", state: storage.SyncPullState("weird_new_state")}
	// Must surface even in non-verbose mode — a bug must never hide behind "pulled".
	var out bytes.Buffer
	if err := printSyncPullOutcome(&out, outcome, false); err != nil {
		t.Fatalf("printSyncPullOutcome error = %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "weird_new_state") || !strings.Contains(text, "bug") {
		t.Fatalf("unknown-state text does not surface the state and flag it as a bug: %q", text)
	}
}

// printSyncPullOutcome renders no prose_pending case: a held conflict is a
// returned SyncFailureError (see TestSyncFailureFromPullHoldsProseConflict and
// the contract-shape tests in sync_failure_test.go), never a printed outcome.
// This pins the guard behind that routing: the printer's enumerated arms map any
// unrecognized pull state to a reported bug, not a bland "pulled". prose_pending
// is the concrete state used here because a prose_pending reaching the printer
// at all would be a routing bug (runSyncPull intercepts it first) — exactly what
// must surface loudly.
func TestPrintSyncPullOutcomeSurfacesUnknownStateAsBug(t *testing.T) {
	t.Parallel()
	outcome := syncPullOutcome{remote: "origin", branch: "master", state: storage.SyncPullState("prose_pending")}
	var out bytes.Buffer
	if err := printSyncPullOutcome(&out, outcome, false); err != nil {
		t.Fatalf("printSyncPullOutcome() error = %v", err)
	}
	if text := out.String(); !strings.Contains(text, "prose_pending") || !strings.Contains(text, "bug") {
		t.Fatalf("stray prose_pending not surfaced as a bug: %q", text)
	}
}

func TestPrintSyncPullOutcomeNoRemoteSkippedText(t *testing.T) {
	t.Parallel()
	outcome := syncPullOutcome{skip: syncTargetNoRemote}
	var out bytes.Buffer
	if err := printSyncPullOutcome(&out, outcome, false); err != nil {
		t.Fatalf("printSyncPullOutcome() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "" {
		t.Fatalf("printSyncPullOutcome() = %q, want empty output", got)
	}
}

func TestPrintSyncPullOutcomeNoRemoteSkippedVerboseText(t *testing.T) {
	t.Parallel()
	outcome := syncPullOutcome{skip: syncTargetNoRemote}
	var out bytes.Buffer
	if err := printSyncPullOutcome(&out, outcome, true); err != nil {
		t.Fatalf("printSyncPullOutcome() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "skipped sync pull: no eligible git remote" {
		t.Fatalf("printSyncPullOutcome() = %q, want verbose no-remote message", got)
	}
}

func TestPrintSyncPushOutcomeNoRemoteSkippedText(t *testing.T) {
	t.Parallel()
	outcome := syncPushOutcome{skip: syncTargetNoRemote}
	var out bytes.Buffer
	if err := printSyncPushOutcome(&out, outcome, false); err != nil {
		t.Fatalf("printSyncPushOutcome() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "" {
		t.Fatalf("printSyncPushOutcome() = %q, want empty output", got)
	}
}

func TestPrintSyncPushOutcomeNoRemoteSkippedVerboseText(t *testing.T) {
	t.Parallel()
	outcome := syncPushOutcome{skip: syncTargetNoRemote}
	var out bytes.Buffer
	if err := printSyncPushOutcome(&out, outcome, true); err != nil {
		t.Fatalf("printSyncPushOutcome() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "no upstream remote and no single configured remote; skipping sync push" {
		t.Fatalf("printSyncPushOutcome() = %q, want verbose no-remote message", got)
	}
}

func TestPrintSyncPushOutcomeRemoteEmptyAlwaysEmitsFirstPushMessage(t *testing.T) {
	t.Parallel()
	outcome := syncPushOutcome{skip: syncTargetRemoteEmpty, remote: "origin"}
	var out bytes.Buffer
	if err := printSyncPushOutcome(&out, outcome, false); err != nil {
		t.Fatalf("printSyncPushOutcome() error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "first push") {
		t.Fatalf("printSyncPushOutcome() = %q, want first-push message", got)
	}
	if !strings.Contains(got, "ONLY") {
		t.Fatalf("printSyncPushOutcome() = %q, want emphasis that skip is only valid on first push", got)
	}
	if !strings.Contains(got, "this message means something is wrong") {
		t.Fatalf("printSyncPushOutcome() = %q, want warning that non-initial skips are a problem", got)
	}
}

func TestPrintSyncPullOutcomeRemoteEmptyAlwaysEmitsFirstPushMessage(t *testing.T) {
	t.Parallel()
	outcome := syncPullOutcome{skip: syncTargetRemoteEmpty, remote: "origin"}
	var out bytes.Buffer
	if err := printSyncPullOutcome(&out, outcome, false); err != nil {
		t.Fatalf("printSyncPullOutcome() error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "first push") {
		t.Fatalf("printSyncPullOutcome() = %q, want first-push message", got)
	}
	if !strings.Contains(got, "ONLY") {
		t.Fatalf("printSyncPullOutcome() = %q, want emphasis that skip is only valid on first push", got)
	}
}

func TestPrintSyncPullOutcomeDefaultSuccessTextHidesRemoteDetails(t *testing.T) {
	t.Parallel()
	outcome := syncPullOutcome{remote: "origin", branch: "master", state: storage.SyncPullFastForwarded}
	var out bytes.Buffer
	if err := printSyncPullOutcome(&out, outcome, false); err != nil {
		t.Fatalf("printSyncPullOutcome() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "pulled" {
		t.Fatalf("printSyncPullOutcome() = %q, want pulled", got)
	}
}

func TestPrintSyncPushOutcomeDefaultSuccessTextHidesRemoteDetails(t *testing.T) {
	t.Parallel()
	outcome := syncPushOutcome{remote: "origin", branch: "master", message: "Pushing to origin"}
	var out bytes.Buffer
	if err := printSyncPushOutcome(&out, outcome, false); err != nil {
		t.Fatalf("printSyncPushOutcome() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "pushed" {
		t.Fatalf("printSyncPushOutcome() = %q, want pushed", got)
	}
}

func TestBuildRemoteSyncChanges(t *testing.T) {
	t.Parallel()
	gitRemotes := []workspace.GitRemote{
		{Name: "origin", URL: "https://example.com/new-origin.git"},
		{Name: "upstream", URL: "https://example.com/upstream.git"},
	}
	doltRemotes := []storage.SyncRemote{
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
	t.Parallel()
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
	t.Parallel()
	remotes := []workspace.GitRemote{{Name: "origin"}, {Name: "upstream"}}
	_, err := resolveSyncRemote("fork", "upstream", remotes)
	if err == nil {
		t.Fatal("resolveSyncRemote() error = nil, want error for unknown requested remote")
	}
}

func TestResolveSyncRemoteUsesUpstreamRemoteWhenPresent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	got, err := resolveSyncBranch(context.Background(), t.TempDir(), "origin")
	if err != nil {
		t.Fatalf("resolveSyncBranch() error = %v", err)
	}
	if got != "debug-branch" {
		t.Fatalf("resolveSyncBranch() = %q, want debug-branch", got)
	}
}

func TestResolveSyncBranchErrorsWhenDefaultBranchUnavailable(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "")
	_, err := resolveSyncBranch(context.Background(), t.TempDir(), "origin")
	if err == nil {
		t.Fatal("expected error when default branch is unavailable")
	}
	if !strings.Contains(err.Error(), debugSyncBranchEnvVar) {
		t.Fatalf("error = %q, want mention of %s", err.Error(), debugSyncBranchEnvVar)
	}
}

// A cancelled context must surface as the branch resolution's true cause, not the
// misleading "default branch unavailable" that DefaultRemoteBranch's swallowed git
// error would otherwise produce. An already-cancelled ctx makes the underlying
// exec.CommandContext return context.Canceled before running git, so DefaultRemoteBranch
// yields "" — the same empty result a genuine no-default gives — and resolveSyncBranch
// is the single point that tells the two apart. [LAW:no-silent-failure]
func TestResolveSyncBranchSurfacesCancellationNotMisleadingUnavailable(t *testing.T) {
	t.Setenv(debugSyncBranchEnvVar, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := resolveSyncBranch(ctx, t.TempDir(), "origin")
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled in the chain", err)
	}
	if strings.Contains(err.Error(), "default branch unavailable") {
		t.Fatalf("cancelled ctx must not surface the misleading unavailable message: %q", err.Error())
	}
}

// TestPrintSyncPushOutcomeSurfacesMaintenanceInBothModes is the check the
// remote-cache prune needs and did not originally have. The prune's whole safety
// story rests on a refusal message reaching the operator when its key derivation
// disagrees with the disk; plumbing that message into the payload and never
// rendering it is the same silence, one layer further down. Both modes are
// asserted because a warning visible only behind --verbose is still silent where
// it counts.
//
// The assertion is on position, not presence. Emitting the line above the
// cascade is what keeps a later arm from forgetting it, and the CHANGELOG
// states that placement as user-visible behavior — so `Contains` would pass for
// exactly the arrangement this test exists to rule out, one where the line has
// slipped back inside a branch and rides along only when that branch is taken.
func TestPrintSyncPushOutcomeSurfacesMaintenanceInBothModes(t *testing.T) {
	t.Parallel()
	const refusal = "remote-cache prune: declining to prune: 3 cache directories match no configured remote"

	// Each mode renders its own push line — plain mode says "pushed", verbose
	// echoes the engine's raw output — and the maintenance line must precede
	// whichever one this mode chose.
	for _, tc := range []struct {
		verbose  bool
		wantPush string
	}{
		{verbose: false, wantPush: "pushed"},
		{verbose: true, wantPush: "Everything up-to-date."},
	} {
		outcome := syncPushOutcome{
			remote:      "origin",
			branch:      "master",
			message:     "Everything up-to-date.",
			maintenance: refusal,
		}
		var out bytes.Buffer
		if err := printSyncPushOutcome(&out, outcome, tc.verbose); err != nil {
			t.Fatalf("printSyncPushOutcome(verbose=%v) error = %v", tc.verbose, err)
		}
		lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		if lines[0] != refusal {
			t.Fatalf("printSyncPushOutcome(verbose=%v) = %q, want the maintenance line first, got %q",
				tc.verbose, out.String(), lines[0])
		}
		if len(lines) < 2 || lines[1] != tc.wantPush {
			t.Fatalf("printSyncPushOutcome(verbose=%v) = %q, want %q to still follow the maintenance line",
				tc.verbose, out.String(), tc.wantPush)
		}
	}
}

// TestPrintSyncPushOutcomeAddsNoLineWhenMaintenanceIsEmpty pins the other half:
// an ordinary push must not grow a line. The engine reports empty when it found
// nothing to collect, and empty must render as nothing at all.
func TestPrintSyncPushOutcomeAddsNoLineWhenMaintenanceIsEmpty(t *testing.T) {
	t.Parallel()
	outcome := syncPushOutcome{
		remote:  "origin",
		branch:  "master",
		message: "Everything up-to-date.",
	}
	var out bytes.Buffer
	if err := printSyncPushOutcome(&out, outcome, true); err != nil {
		t.Fatalf("printSyncPushOutcome() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "Everything up-to-date." {
		t.Fatalf("printSyncPushOutcome() = %q, want only the push line", got)
	}
}

// TestSyncPushTraceMetadataCarriesTheMaintenanceReport is the check the durable
// trace needed and did not have. `lit sync push` backs the pre-push hook, where
// stdout is routinely swallowed, so the trace is the channel a prune refusal
// actually survives on — and until this job had a name, asserting anything about
// it meant standing up a workspace, a ref-carrying remote and a live engine
// session, which is why the line shipped unasserted.
func TestSyncPushTraceMetadataCarriesTheMaintenanceReport(t *testing.T) {
	t.Parallel()
	const refusal = "remote-cache prune: declining to prune: 3 cache directories match no configured remote"

	metadata := syncPushTraceMetadata("origin", "master", storage.SyncPushResult{
		Message:     "Everything up-to-date.",
		Maintenance: refusal,
	}, nil)

	if metadata["maintenance"] != refusal {
		t.Fatalf("metadata[maintenance] = %q, want the refusal %q", metadata["maintenance"], refusal)
	}
	if metadata["message"] != "Everything up-to-date." {
		t.Fatalf("metadata[message] = %q, want the engine's push output", metadata["message"])
	}
	if metadata["remote"] != "origin" || metadata["sync_branch"] != "master" {
		t.Fatalf("metadata = %v, want remote/sync_branch preserved", metadata)
	}
	if _, present := metadata["error"]; present {
		t.Fatalf("metadata = %v, want no error key on a push that succeeded", metadata)
	}
}

// TestSyncPushTraceMetadataOmitsWhatDidNotHappen pins the other half: the trace
// says nothing about maintenance on an ordinary push, so a reader scanning
// records for the key finds it exactly when there was something to report.
func TestSyncPushTraceMetadataOmitsWhatDidNotHappen(t *testing.T) {
	t.Parallel()
	metadata := syncPushTraceMetadata("origin", "master", storage.SyncPushResult{
		Message: "Everything up-to-date.",
	}, nil)
	if _, present := metadata["maintenance"]; present {
		t.Fatalf("metadata = %v, want no maintenance key when the prune found nothing", metadata)
	}

	failed := syncPushTraceMetadata("origin", "master", storage.SyncPushResult{}, errors.New("push rejected"))
	if failed["error"] != "push rejected" {
		t.Fatalf("metadata[error] = %q, want the push failure recorded", failed["error"])
	}
	if _, present := failed["message"]; present {
		t.Fatalf("metadata = %v, want no message key when the engine said nothing", failed)
	}
}
