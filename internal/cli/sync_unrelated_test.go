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
