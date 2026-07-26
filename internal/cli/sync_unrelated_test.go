package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

// TestSyncFailureBlockUnrelatedHistories pins the contract the unrelated-histories
// class renders: the four standing elements plus a domain WHAT that names the
// no-common-ancestor reality, resolution steps that name only REAL commands (no
// invented take-one/union command that does not exist yet), and a BLOCKED
// escalation — because unrelated histories, like a schema-ahead remote, never merge
// on a retry.
func TestSyncFailureBlockUnrelatedHistories(t *testing.T) {
	failure := SyncFailure{
		Class:  syncFailureUnrelatedHistories,
		Remote: "origin",
		Branch: "master",
		Ahead:  7,
		Behind: 7,
	}
	block := failure.blockString()
	assertContractElements(t, block, "lit doctor")

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
	// The resolution steps must not promise a command that does not exist yet (the
	// take-one/union surface is the rest of the epic). [LAW:no-silent-failure]
	if strings.Contains(block, "lit sync reconcile take") || strings.Contains(block, "lit sync reconcile combine") {
		t.Errorf("block names an unbuilt resolution command:\n%s", block)
	}
}

// TestSyncFailureBlockUnrelatedInventory pins the both-sides partition rendering:
// when the failure carries an inventory, the block enumerates each side's issue ids
// under its own labeled cell, an empty side reads as an explicit "(0)", and a
// non-empty side names its count and members. This is the ticket's visibility — see
// what each side holds before choosing take-one or union.
func TestSyncFailureBlockUnrelatedInventory(t *testing.T) {
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
	var sink strings.Builder
	err := reportReconcileResult(&sink, "origin", "master", store.SyncReconcileResult{
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
	var sink strings.Builder
	err := reportReconcileResult(&sink, "origin", "master", store.SyncReconcileResult{
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
}

// TestSyncFailureFromPullUnrelated proves `lit sync pull` surfaces an
// unrelated-histories pull outcome through the same contract and exit as
// `lit sync reconcile`, so the two never disagree on the same divergence.
func TestSyncFailureFromPullUnrelated(t *testing.T) {
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
