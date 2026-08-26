package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestSyncFailureBlockUnrelatedHistories pins the contract the unrelated-histories
// class renders: the four standing elements plus a domain WHAT that names the
// no-common-ancestor reality, resolution steps that name ALL THREE now-built
// resolutions (both wholesale takes AND the union `combine`), and a BLOCKED
// escalation — because unrelated histories, like a schema-ahead remote, never merge
// on a retry; only a deliberate choice resolves them.
func TestSyncFailureBlockUnrelatedHistories(t *testing.T) {
	t.Parallel()
	failure := SyncFailure{
		Class:  syncFailureUnrelatedHistories,
		Remote: "origin",
		Branch: "master",
		Ahead:  7,
		Behind: 7,
	}
	block := failure.blockString()
	assertContractElements(t, block, "lit sync reconcile take", "lit sync reconcile combine")

	// WHAT names the no-common-ancestor reality in domain terms, not a backend string.
	for _, want := range []string{"no common history", "no shared ancestor", "wholesale"} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing unrelated-histories phrasing %q:\n%s", want, block)
		}
	}
	// Escalation is the fixed BLOCKED framing — never the age-based "recent, still
	// routine" line that would invite a retry that can never merge.
	if !strings.Contains(block, "BLOCKED") {
		t.Errorf("unrelated-histories escalation is not BLOCKED:\n%s", block)
	}
	if strings.Contains(block, "still within the window where a divergence is routine") {
		t.Errorf("unrelated-histories used the routine/aged escalation, which invites an impossible retry:\n%s", block)
	}
	// All three resolutions exist now (take local/remote and combine) and each must be
	// named so the agent can act; combine is the keep-everything option and must be
	// present, no longer deferred as "not yet automated". [LAW:no-silent-failure]
	for _, want := range []string{"lit sync reconcile take", "lit sync reconcile combine"} {
		if !strings.Contains(block, want) {
			t.Errorf("block does not name the now-built resolution %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "not yet automated") {
		t.Errorf("block still defers the union as not-yet-automated after combine shipped:\n%s", block)
	}
}

// TestSyncFailureBlockUnrelatedInventory pins the both-sides partition rendering:
// when the failure carries an inventory, the block enumerates each side's issue ids
// under its own labeled cell, an empty side reads as an explicit "(0)", and a
// non-empty side names its count and members. This is the ticket's visibility — see
// what each side holds before choosing take-one or union.
func TestSyncFailureBlockUnrelatedInventory(t *testing.T) {
	t.Parallel()
	failure := SyncFailure{
		Class:  syncFailureUnrelatedHistories,
		Remote: "origin",
		Branch: "master",
		Ahead:  3,
		Behind: 2,
		Inventory: &store.UnrelatedInventory{
			OnlyLocal:  []string{"proj-local1", "proj-local2"},
			OnlyRemote: []string{"proj-remote1"},
			OnBoth:     []string{"proj-shared1"},
		},
	}
	block := failure.blockString()

	if !strings.Contains(block, "WHAT EACH SIDE HOLDS") {
		t.Fatalf("block missing the both-sides inventory section:\n%s", block)
	}
	for _, want := range []string{
		"only on local:  (2): proj-local1, proj-local2",
		"only on remote: (1): proj-remote1",
		"on both:        (1): proj-shared1",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("inventory section missing %q:\n%s", want, block)
		}
	}
}

// TestSyncFailureBlockUnrelatedInventoryEmptySide proves a side with no issues
// renders as an explicit "(0)" rather than a blank the reader must interpret, and
// that a nil inventory (any other class) emits no inventory section at all.
func TestSyncFailureBlockUnrelatedInventoryEmptySide(t *testing.T) {
	t.Parallel()
	withEmpty := SyncFailure{
		Class:  syncFailureUnrelatedHistories,
		Remote: "origin",
		Branch: "master",
		Inventory: &store.UnrelatedInventory{
			OnlyLocal:  []string{"proj-local1"},
			OnlyRemote: nil,
			OnBoth:     nil,
		},
	}
	block := withEmpty.blockString()
	if !strings.Contains(block, "only on remote: (0)") {
		t.Errorf("empty remote side did not render as (0):\n%s", block)
	}
	if !strings.Contains(block, "on both:        (0)") {
		t.Errorf("empty on-both did not render as (0):\n%s", block)
	}

	// A class with no inventory emits no inventory section — the loop yields nothing.
	noInventory := SyncFailure{Class: syncFailureUnrelatedHistories, Remote: "origin", Branch: "master"}
	if strings.Contains(noInventory.blockString(), "WHAT EACH SIDE HOLDS") {
		t.Errorf("a failure with no inventory still rendered the section:\n%s", noInventory.blockString())
	}
}

// TestReportReconcileResultUnrelatedCarriesInventory proves the reconcile surface
// forwards the store's both-sides partition into the rendered contract, so the
// operator sees what each side holds on `lit sync reconcile`.
func TestReportReconcileResultUnrelatedCarriesInventory(t *testing.T) {
	t.Parallel()
	var sink strings.Builder
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	err := reportReconcileResult(context.Background(), &sink, ws, nil, "lit sync reconcile", "origin", "master", store.SyncReconcileResult{
		State:  store.SyncReconcileUnrelated,
		Ahead:  1,
		Behind: 1,
		Unrelated: &store.UnrelatedInventory{
			OnlyLocal:  []string{"proj-onlylocal"},
			OnlyRemote: []string{"proj-onlyremote"},
		},
	}, false)
	if err == nil {
		t.Fatalf("reportReconcileResult returned nil for an unrelated result")
	}
	for _, want := range []string{"proj-onlylocal", "proj-onlyremote", "WHAT EACH SIDE HOLDS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("reconcile contract missing %q:\n%s", want, err.Error())
		}
	}
}

// TestSyncFailureFromPullUnrelatedCarriesInventory proves `lit sync pull` forwards
// the same partition through the same contract, so pull and reconcile show the
// identical both-sides view of one divergence.
func TestSyncFailureFromPullUnrelatedCarriesInventory(t *testing.T) {
	t.Parallel()
	inv := &store.UnrelatedInventory{OnlyLocal: []string{"proj-l"}, OnBoth: []string{"proj-b"}}
	failure, held := syncFailureFromPull("origin", "master", store.SyncPullResult{
		State:     store.SyncPullUnrelated,
		Unrelated: inv,
	}, time.Now())
	if !held {
		t.Fatalf("syncFailureFromPull returned held=false for an unrelated pull")
	}
	if failure.Failure.Inventory != inv {
		t.Fatalf("pull failure did not carry the store's inventory: got %v, want %v", failure.Failure.Inventory, inv)
	}
}

// TestReportReconcileResultUnrelatedExitsConflict proves the reconcile surface maps
// an unrelated-histories result to the one sync-failure contract at ExitConflict —
// the same exit a held prose conflict gives — rather than a bland success line.
func TestReportReconcileResultUnrelatedExitsConflict(t *testing.T) {
	t.Parallel()
	var sink strings.Builder
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	err := reportReconcileResult(context.Background(), &sink, ws, nil, "lit sync reconcile", "origin", "master", store.SyncReconcileResult{
		State:  store.SyncReconcileUnrelated,
		Ahead:  7,
		Behind: 7,
	}, false)
	if err == nil {
		t.Fatalf("reportReconcileResult returned nil for an unrelated-histories result; want a returned failure")
	}
	var failure SyncFailureError
	if !errors.As(err, &failure) {
		t.Fatalf("error type = %T, want SyncFailureError", err)
	}
	if code := ExitCode(err); code != ExitConflict {
		t.Fatalf("unrelated reconcile exit code = %d, want %d (ExitConflict)", code, ExitConflict)
	}
	if !strings.Contains(err.Error(), "no common history") {
		t.Fatalf("contract does not name unrelated histories:\n%s", err.Error())
	}
	if failure.Failure.BuildNote == "" {
		t.Fatal("reportReconcileResult did not resolve BuildNote")
	}
}

// TestReportTakeOutcomeReportsDiscard proves the take-one surface names the discarded
// side's unique issues explicitly — take-remote reports the local-only issues it drops,
// take-local the remote-only ones — so the discard is reported, never silent, which is
// the ticket's core guarantee. [LAW:no-silent-failure]
func TestReportTakeOutcomeReportsDiscard(t *testing.T) {
	t.Parallel()
	inv := &store.UnrelatedInventory{
		OnlyLocal:  []string{"proj-localA", "proj-localB"},
		OnlyRemote: []string{"proj-remoteA"},
		OnBoth:     []string{"proj-shared"},
	}
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}

	var remote strings.Builder
	if err := reportTakeOutcome(&remote, ws, "lit sync reconcile take remote", "origin", "master", store.SyncReconcileResult{
		State:     store.SyncReconcileTookRemote,
		Unrelated: inv,
	}); err != nil {
		t.Fatalf("reportTakeOutcome(TookRemote): %v", err)
	}
	remoteOut := remote.String()
	if !strings.Contains(remoteOut, "DISCARDED") {
		t.Errorf("take-remote output does not report a discard:\n%s", remoteOut)
	}
	// It names the LOCAL-only issues (the ones take-remote drops), not the remote-only ones.
	for _, want := range []string{"proj-localA", "proj-localB"} {
		if !strings.Contains(remoteOut, want) {
			t.Errorf("take-remote discard did not name %q:\n%s", want, remoteOut)
		}
	}
	if strings.Contains(remoteOut, "proj-remoteA") {
		t.Errorf("take-remote reported a remote-only id as discarded (kept side):\n%s", remoteOut)
	}

	var local strings.Builder
	if err := reportTakeOutcome(&local, ws, "lit sync reconcile take local", "origin", "master", store.SyncReconcileResult{
		State:     store.SyncReconcileTookLocal,
		Unrelated: inv,
	}); err != nil {
		t.Fatalf("reportTakeOutcome(TookLocal): %v", err)
	}
	localOut := local.String()
	if !strings.Contains(localOut, "proj-remoteA") {
		t.Errorf("take-local discard did not name the remote-only id proj-remoteA:\n%s", localOut)
	}
	if strings.Contains(localOut, "proj-localA") {
		t.Errorf("take-local reported a local-only id as discarded (kept side):\n%s", localOut)
	}
}

// TestReportTakeOutcomeEmptyDiscard proves that when the chosen side drops nothing, the
// output states an explicit "(0)" rather than a blank the reader must interpret.
func TestReportTakeOutcomeEmptyDiscard(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	if err := reportTakeOutcome(&out, ws, "lit sync reconcile take remote", "origin", "master", store.SyncReconcileResult{
		State:     store.SyncReconcileTookRemote,
		Unrelated: &store.UnrelatedInventory{OnlyRemote: []string{"proj-remoteA"}},
	}); err != nil {
		t.Fatalf("reportTakeOutcome: %v", err)
	}
	if !strings.Contains(out.String(), "(0)") {
		t.Errorf("an empty discard did not render as (0):\n%s", out.String())
	}
}

// TestParseUnrelatedSide pins the take command's side mapping: the two real sides map
// to their store resolution, and an unknown word is a usage error, never a silent
// default. [LAW:no-silent-failure]
func TestParseUnrelatedSide(t *testing.T) {
	t.Parallel()
	for word, want := range map[string]store.UnrelatedResolution{
		"local": store.TakeLocal, "remote": store.TakeRemote,
		"LOCAL": store.TakeLocal, " remote ": store.TakeRemote,
	} {
		got, err := parseUnrelatedSide(word)
		if err != nil {
			t.Errorf("parseUnrelatedSide(%q) errored: %v", word, err)
		}
		if got != want {
			t.Errorf("parseUnrelatedSide(%q) = %q, want %q", word, got, want)
		}
	}
	for _, bad := range []string{"", "mine", "theirs", "combine", "both"} {
		if _, err := parseUnrelatedSide(bad); err == nil {
			t.Errorf("parseUnrelatedSide(%q) returned nil error, want a usage error", bad)
		}
	}
}

// TestSyncFailureFromPullUnrelated proves `lit sync pull` surfaces an
// unrelated-histories pull outcome through the same contract and exit as
// `lit sync reconcile`, so the two never disagree on the same divergence.
func TestSyncFailureFromPullUnrelated(t *testing.T) {
	t.Parallel()
	failure, held := syncFailureFromPull("origin", "master", store.SyncPullResult{
		State:  store.SyncPullUnrelated,
		Ahead:  7,
		Behind: 7,
	}, time.Now())
	if !held {
		t.Fatalf("syncFailureFromPull returned held=false for an unrelated pull; want held=true")
	}
	if failure.Failure.Class != syncFailureUnrelatedHistories {
		t.Fatalf("pull failure class = %q, want %q", failure.Failure.Class, syncFailureUnrelatedHistories)
	}
	if code := ExitCode(failure); code != ExitConflict {
		t.Fatalf("unrelated pull exit code = %d, want %d (ExitConflict)", code, ExitConflict)
	}
}
