package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
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

func TestPrintSyncPullOutcomeNeverSyncedDirectsUpstreamSetup(t *testing.T) {
	t.Parallel()
	// A branch the remote has never seen is the typed never_synced state, not a
	// parsed backend error string — the printer directs the caller to set the
	// upstream with a deterministic command derived from the resolved remote.
	outcome := syncPullOutcome{remote: remoteName(t, "origin"), branch: "feature/local-only", state: storage.SyncPullNeverSynced}
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

// A held free-text conflict routes through the one sync-failure contract as a
// returned error, so `lit sync pull` exits ExitConflict like `lit sync reconcile`
// does for the identical state.
// syncFailureFromPull is the pure mapping the command uses; this pins that a
// prose-pending pull yields the proseHeld contract and every non-held state does not.
func TestSyncFailureFromPullHoldsProseConflict(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	failure, held := syncFailureFromPull(remoteName(t, "origin"), "master", storage.SyncPullResult{
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
		if _, held := syncFailureFromPull(remoteName(t, "origin"), "master", storage.SyncPullResult{State: state}, now); held {
			t.Fatalf("state %q wrongly classified as a held conflict", state)
		}
	}
}

func TestPrintSyncPullOutcomeNeverSyncedWithoutVerboseOmitsRemoteDetails(t *testing.T) {
	t.Parallel()
	outcome := syncPullOutcome{remote: remoteName(t, "origin"), branch: "feature/local-only", state: storage.SyncPullNeverSynced}
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
		outcome := syncPullOutcome{remote: remoteName(t, "origin"), branch: "master", state: state}
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
	outcome := syncPullOutcome{remote: remoteName(t, "origin"), branch: "master", state: storage.SyncPullState("weird_new_state")}
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
	outcome := syncPullOutcome{remote: remoteName(t, "origin"), branch: "master", state: storage.SyncPullState("prose_pending")}
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
	outcome := syncPushOutcome{skip: syncTargetRemoteEmpty, remote: remoteName(t, "origin")}
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
	outcome := syncPullOutcome{skip: syncTargetRemoteEmpty, remote: remoteName(t, "origin")}
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
	outcome := syncPullOutcome{remote: remoteName(t, "origin"), branch: "master", state: storage.SyncPullFastForwarded}
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
	outcome := syncPushOutcome{remote: remoteName(t, "origin"), branch: "master", message: "Pushing to origin"}
	var out bytes.Buffer
	if err := printSyncPushOutcome(&out, outcome, false); err != nil {
		t.Fatalf("printSyncPushOutcome() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "pushed" {
		t.Fatalf("printSyncPushOutcome() = %q, want pushed", got)
	}
}

func TestPlanRemoteSync(t *testing.T) {
	t.Parallel()
	gitRemotes := []workspace.GitRemote{
		gitRemoteAt("origin", "https://example.com/new-origin.git"),
		gitRemoteAt("upstream", "https://example.com/upstream.git"),
		gitRemoteAt("a/b", "https://example.com/a-b.git"),
		gitRemoteAt("old.fork", "https://example.com/old-fork.git"),
		gitRemoteAt("linked", "link/../remote.git"),
	}
	doltRemotes := []storage.SyncRemote{
		{Name: "origin", URL: "https://example.com/old-origin.git"},
		{Name: "fork", URL: "https://example.com/fork.git"},
		{Name: "linked", URL: "git+file:///elsewhere/remote.git"},
	}

	got, err := planRemoteSync(gitRemotes, doltRemotes)
	if err != nil {
		t.Fatalf("planRemoteSync() error = %v", err)
	}
	// A remote whose URL is refused is not wanted, so the Dolt remote of its
	// name goes, as for a name Dolt cannot hold.
	want := remoteSyncPlan{
		add:    []plannedRemote{{name: remoteName(t, "upstream"), url: "git+https://example.com/upstream.git"}},
		update: []plannedRemote{{name: remoteName(t, "origin"), url: "git+https://example.com/new-origin.git"}},
		remove: []string{"fork", "linked"},
		refused: []storage.RefusedRemoteError{
			{Name: "a/b", Reason: storage.NameCharacter{Character: "/"}},
			{Name: "linked", Reason: storage.PathThroughFilesystem{URL: "link/../remote.git"}},
			{Name: "old.fork", Reason: storage.NameCharacter{Character: "."}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planRemoteSync() = %#v, want %#v", got, want)
	}
}

// gitRemote is a git remote named name, with a URL nothing refuses.
func gitRemote(name string) workspace.GitRemote {
	return gitRemoteAt(name, "https://example.com/"+name+".git")
}

// gitRemoteAt is a git remote named name with the fetch URL written. Its root
// is the zero CheckoutRoot, so a URL that would be joined panics: a fixture
// that needs a join builds its remote from a real checkout.
func gitRemoteAt(name, written string) workspace.GitRemote {
	return workspace.NewGitRemote(name, written, workspace.CheckoutRoot{})
}

// A chosen remote Dolt cannot hold is refused however it was chosen, never
// passed over for another remote.
func TestResolveSyncRemoteRefusesARemoteDoltCannotHold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		requested, upstream string
		remotes             []string
	}{
		{name: "requested", requested: "a/b", upstream: "origin", remotes: []string{"origin", "a/b"}},
		{name: "upstream", upstream: "a/b", remotes: []string{"origin", "a/b"}},
		{name: "single", remotes: []string{"a/b"}},
	} {
		remotes := make([]workspace.GitRemote, 0, len(tc.remotes))
		for _, name := range tc.remotes {
			remotes = append(remotes, gitRemote(name))
		}
		got, ok, err := resolveSyncRemote(tc.requested, upstreamIs(tc.upstream), remotes)
		if err == nil || !strings.Contains(err.Error(), `remote "a/b" has "/" in its name`) {
			t.Errorf("%s: resolveSyncRemote() = %q, %t, %v; want the refusal naming a/b", tc.name, got.Name(), ok, err)
		}
	}
}

// upstreamIs is an upstream read that answers name.
func upstreamIs(name string) func() (string, error) {
	return func() (string, error) { return name, nil }
}

// The upstream is asked only when its answer can decide, so a workspace with
// a request, no remote, or one remote spends no git on it, and a git that
// could not answer cannot fail a choice it would not change.
func TestResolveSyncRemoteAsksUpstreamOnlyWhenItDecides(t *testing.T) {
	t.Parallel()
	unanswerable := func() (string, error) { return "", errors.New("for-each-ref failed") }
	two := []workspace.GitRemote{gitRemote("origin"), gitRemote("upstream")}
	for _, tc := range []struct {
		name      string
		requested string
		remotes   []workspace.GitRemote
		want      string
	}{
		{name: "a request", requested: "upstream", remotes: two, want: "upstream"},
		{name: "no remote", remotes: nil},
		{name: "one remote", remotes: []workspace.GitRemote{gitRemote("origin")}, want: "origin"},
	} {
		got, ok, err := resolveSyncRemote(tc.requested, unanswerable, tc.remotes)
		if err != nil || ok != (tc.want != "") || (ok && got.Name() != remoteName(t, tc.want)) {
			t.Errorf("%s: resolveSyncRemote() = %q, %t, %v; want %q without asking the upstream", tc.name, got.Name(), ok, err, tc.want)
		}
	}
	if _, _, err := resolveSyncRemote("", unanswerable, two); err == nil {
		t.Error("with two remotes and no request, an upstream git cannot answer must fail the choice, never fall to none")
	}
}

func TestResolveSyncRemoteUsesRequestedRemoteFirst(t *testing.T) {
	t.Parallel()
	remotes := []workspace.GitRemote{gitRemote("origin"), gitRemote("upstream")}
	got, ok, err := resolveSyncRemote("origin", upstreamIs("upstream"), remotes)
	if err != nil {
		t.Fatalf("resolveSyncRemote() error = %v", err)
	}
	if !ok || got.Name() != remoteName(t, "origin") {
		t.Fatalf("resolveSyncRemote() = %q, %t, want origin", got.Name(), ok)
	}
}

func TestResolveSyncRemoteErrorsWhenRequestedRemoteIsUnknown(t *testing.T) {
	t.Parallel()
	remotes := []workspace.GitRemote{gitRemote("origin"), gitRemote("upstream")}
	_, _, err := resolveSyncRemote("fork", upstreamIs("upstream"), remotes)
	if err == nil {
		t.Fatal("resolveSyncRemote() error = nil, want error for unknown requested remote")
	}
}

func TestResolveSyncRemoteUsesUpstreamRemoteWhenPresent(t *testing.T) {
	t.Parallel()
	remotes := []workspace.GitRemote{gitRemote("origin"), gitRemote("upstream")}
	got, ok, err := resolveSyncRemote("", upstreamIs("upstream"), remotes)
	if err != nil {
		t.Fatalf("resolveSyncRemote() error = %v", err)
	}
	if !ok || got.Name() != remoteName(t, "upstream") {
		t.Fatalf("resolveSyncRemote() = %q, %t, want upstream", got.Name(), ok)
	}
}

func TestResolveSyncRemoteUsesSingleRemoteFallback(t *testing.T) {
	t.Parallel()
	remotes := []workspace.GitRemote{gitRemote("origin")}
	got, ok, err := resolveSyncRemote("", upstreamIs(""), remotes)
	if err != nil {
		t.Fatalf("resolveSyncRemote() error = %v", err)
	}
	if !ok || got.Name() != remoteName(t, "origin") {
		t.Fatalf("resolveSyncRemote() = %q, %t, want origin", got.Name(), ok)
	}
}

func TestResolveSyncRemoteIgnoresUnknownUpstreamRemote(t *testing.T) {
	t.Parallel()
	remotes := []workspace.GitRemote{gitRemote("origin"), gitRemote("upstream")}
	got, ok, err := resolveSyncRemote("", upstreamIs("missing"), remotes)
	if err != nil {
		t.Fatalf("resolveSyncRemote() error = %v", err)
	}
	if ok {
		t.Fatalf("resolveSyncRemote() = %q, want none", got.Name())
	}
}

func TestResolveSyncRemoteReturnsEmptyWhenNoEligibleRemote(t *testing.T) {
	t.Parallel()
	got, ok, err := resolveSyncRemote("", upstreamIs(""), nil)
	if err != nil {
		t.Fatalf("resolveSyncRemote() error = %v", err)
	}
	if ok {
		t.Fatalf("resolveSyncRemote() = %q, want none", got.Name())
	}
}

// TestPrintSyncPushOutcomeSurfacesMaintenanceInBothModes is the check the
// remote-cache prune needs. A prune that failed says so only on this line;
// plumbing that message into the payload and never rendering it is the same
// silence, one layer further down. Both modes are asserted because a warning
// visible only behind --verbose is still silent where it counts.
//
// The assertion is on position, not presence. Emitting the line above the
// cascade is what keeps a later arm from forgetting it, and the CHANGELOG
// states that placement as user-visible behavior — so `Contains` would pass for
// exactly the arrangement this test exists to rule out, one where the line has
// slipped back inside a branch and rides along only when that branch is taken.
func TestPrintSyncPushOutcomeSurfacesMaintenanceInBothModes(t *testing.T) {
	t.Parallel()
	const failure = "remote-cache prune: removed 1 abandoned mirror (2.0 KiB), then failed: remove abandoned mirror ab12: permission denied"

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
			remote:      remoteName(t, "origin"),
			branch:      "master",
			message:     "Everything up-to-date.",
			maintenance: failure,
		}
		var out bytes.Buffer
		if err := printSyncPushOutcome(&out, outcome, tc.verbose); err != nil {
			t.Fatalf("printSyncPushOutcome(verbose=%v) error = %v", tc.verbose, err)
		}
		lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		if lines[0] != failure {
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
		remote:  remoteName(t, "origin"),
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
// trace needs. `lit sync push` backs the pre-push hook, where stdout is
// routinely swallowed, so the trace is the channel a prune failure actually
// survives on.
func TestSyncPushTraceMetadataCarriesTheMaintenanceReport(t *testing.T) {
	t.Parallel()
	const failure = "remote-cache prune: removed 1 abandoned mirror (2.0 KiB), then failed: remove abandoned mirror ab12: permission denied"

	metadata := syncPushTraceMetadata(remoteName(t, "origin"), "master", storage.SyncPushResult{
		Message:     "Everything up-to-date.",
		Maintenance: failure,
	}, nil)

	if metadata["maintenance"] != failure {
		t.Fatalf("metadata[maintenance] = %q, want the failure %q", metadata["maintenance"], failure)
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
	metadata := syncPushTraceMetadata(remoteName(t, "origin"), "master", storage.SyncPushResult{
		Message: "Everything up-to-date.",
	}, nil)
	if _, present := metadata["maintenance"]; present {
		t.Fatalf("metadata = %v, want no maintenance key when the prune found nothing", metadata)
	}

	failed := syncPushTraceMetadata(remoteName(t, "origin"), "master", storage.SyncPushResult{}, errors.New("push rejected"))
	if failed["error"] != "push rejected" {
		t.Fatalf("metadata[error] = %q, want the push failure recorded", failed["error"])
	}
	if _, present := failed["message"]; present {
		t.Fatalf("metadata = %v, want no message key when the engine said nothing", failed)
	}
}

// recordingSyncer is a Syncer whose remote list is held in memory and whose
// remote verbs record the order they ran in. Any other method panics, so a
// reconcile that reaches past the remotes fails the test.
type recordingSyncer struct {
	storage.Syncer
	remotes []storage.SyncRemote
	calls   []string
}

func (r *recordingSyncer) SyncListRemotes(context.Context) ([]storage.SyncRemote, error) {
	return slices.Clone(r.remotes), nil
}

func (r *recordingSyncer) SyncAddRemote(_ context.Context, name storage.RemoteName, url string) error {
	r.calls = append(r.calls, "add "+name.String())
	r.remotes = append(r.remotes, storage.SyncRemote{Name: name.String(), URL: url})
	return nil
}

func (r *recordingSyncer) SyncSetRemoteURL(_ context.Context, name storage.RemoteName, url string) error {
	r.calls = append(r.calls, "update "+name.String())
	return nil
}

func (r *recordingSyncer) SyncRemoveRemote(_ context.Context, name string) error {
	r.calls = append(r.calls, "remove "+name)
	r.remotes = slices.DeleteFunc(r.remotes, func(remote storage.SyncRemote) bool { return remote.Name == name })
	return nil
}

// TestTheReconcileRemovesBeforeItAdds: git renamed team/origin, a name an
// older lit cloned under, to team. The reconcile removes team/origin before it
// adds team, so the removal finds no team and takes team/origin's refs with it.
func TestTheReconcileRemovesBeforeItAdds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "remote", "add", "team", "https://example.com/team.git")
	runGit(t, dir, "remote", "add", "fork", "https://example.com/fork-new.git")
	syncer := &recordingSyncer{remotes: []storage.SyncRemote{
		{Name: "team/origin", URL: "git+https://example.com/team.git"},
		{Name: "fork", URL: "git+https://example.com/fork-old.git"},
	}}
	ctx := telemetry.With(context.Background(), telemetry.Begin(telemetry.Event{}))
	if err := syncDoltRemotesFromGit(ctx, syncSession{syncer: syncer}, workspace.Info{RootDir: dir, Remotes: workspace.NewRemotes(testEnv(), repositoryOf(t, dir), checkoutRoot(t, dir)), Project: testProject(checkoutRoot(t, dir))}); err != nil {
		t.Fatalf("syncDoltRemotesFromGit() error = %v", err)
	}
	if want := []string{"remove team/origin", "update fork", "add team"}; !slices.Equal(syncer.calls, want) {
		t.Fatalf("reconcile ran %v, want %v", syncer.calls, want)
	}
}
