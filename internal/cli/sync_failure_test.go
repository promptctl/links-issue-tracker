package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// assertContractElements pins that a rendered sync-failure block carries all four
// elements the contract promises: the unmissable directive, the domain-term WHAT,
// the ordered HOW commands, and a value-driven escalation line. Mirrors the style
// of TestUnsupportedSchemaVersionMessageShape — the shape is a contract, tested
// directly so it cannot silently regress toward the old ignorable one-liner.
func assertContractElements(t *testing.T, block string, wantCommands ...string) {
	t.Helper()
	// (1) Directive — the standing not-ambient-noise fact.
	if !strings.Contains(block, "blocking condition") {
		t.Errorf("block missing the blocking-condition directive:\n%s", block)
	}
	if !strings.Contains(block, "surface it to the user as blocking") {
		t.Errorf("block missing the escalate-to-user instruction:\n%s", block)
	}
	// (2) What happened, in domain terms.
	if !strings.Contains(block, "WHAT HAPPENED:") {
		t.Errorf("block missing the WHAT HAPPENED section:\n%s", block)
	}
	// (3) How to resolve — the exact commands, in order.
	if !strings.Contains(block, "HOW TO RESOLVE") {
		t.Errorf("block missing the HOW TO RESOLVE section:\n%s", block)
	}
	for _, cmd := range wantCommands {
		if !strings.Contains(block, cmd) {
			t.Errorf("block missing resolution command %q:\n%s", cmd, block)
		}
	}
	// (4) Escalation — always present, one of the two severity framings.
	if !strings.Contains(block, "ESCALATION") {
		t.Errorf("block missing the ESCALATION line:\n%s", block)
	}
	// The block is an agent-facing directive envelope, not a bare backend string.
	if !strings.HasPrefix(block, "<agent-instructions>") {
		t.Errorf("block is not wrapped as agent-instructions:\n%s", block)
	}
	// Regression lock: the exact ignorable shrug the 2026-07-08 incident printed
	// must never come back as the failure surface. [LAW:no-silent-failure]
	if strings.Contains(block, "will retry") {
		t.Errorf("block reintroduced the ignorable 'will retry' framing:\n%s", block)
	}
}

func TestSyncFailureBlockProseHeld(t *testing.T) {
	failure := SyncFailure{
		Class:  syncFailureProseHeld,
		Remote: "origin",
		Branch: "master",
		Ahead:  1,
		Behind: 1,
		Age:    2 * time.Hour,
		Fields: []merge.ProsePending{
			{IssueID: "links-x.1", Field: merge.ProseTitle},
			{IssueID: "links-y.2", Field: merge.ProseDescription},
		},
	}
	block := failure.blockString()
	assertContractElements(t, block, "lit sync reconcile")
	// WHAT names the held fields so the agent knows what it owns before opening
	// the workbench, and says it will NOT clear on its own (the engine holds).
	for _, want := range []string{"origin/master", "links-x.1", "links-y.2", "will NOT clear on its own"} {
		if !strings.Contains(block, want) {
			t.Errorf("prose-held WHAT missing %q:\n%s", want, block)
		}
	}
}

func TestSyncFailureBlockDivergedUnresolvedWithCause(t *testing.T) {
	backend := errors.New(`table "i" does not have column "resolution"`)
	failure := SyncFailure{
		Class:  syncFailureDivergedUnresolved,
		Remote: "origin",
		Branch: "master",
		Ahead:  41,
		Behind: 5,
		Age:    5 * 24 * time.Hour,
		Cause:  backend,
	}
	block := failure.blockString()
	assertContractElements(t, block, "lit sync pull", "lit sync reconcile")
	// The backend error is preserved but DEMOTED below a cause label — never the
	// headline. The directive must sit above it. [FRAMING:representation]
	causeIdx := strings.Index(block, backend.Error())
	if causeIdx < 0 {
		t.Fatalf("diverged-unresolved block dropped the backend cause entirely:\n%s", block)
	}
	if directiveIdx := strings.Index(block, "blocking condition"); directiveIdx > causeIdx {
		t.Errorf("backend cause appears above the directive (reads as the headline):\n%s", block)
	}
	if !strings.Contains(block, "cause (backend detail") {
		t.Errorf("backend cause not labeled as demoted detail:\n%s", block)
	}
	// Domain terms: the commit counts, not just the backend string.
	for _, want := range []string{"41 local", "5 remote"} {
		if !strings.Contains(block, want) {
			t.Errorf("diverged WHAT missing domain count %q:\n%s", want, block)
		}
	}
}

// The escalation line is selected by the divergence's VALUES — age and commit
// span — not by the surface that observed it. Either signal alone trips the
// incident framing. [LAW:dataflow-not-control-flow]
func TestSyncFailureEscalationByValue(t *testing.T) {
	cases := []struct {
		name         string
		age          time.Duration
		ahead        int64
		behind       int64
		wantIncident bool
		wantAge      string
	}{
		{"fresh: recent and small", 30 * time.Minute, 1, 1, false, "30 minutes"},
		{"persistent by age alone", 25 * time.Hour, 1, 0, true, "25 hours"},
		{"persistent by many days", 5 * 24 * time.Hour, 2, 1, true, "5 days"},
		{"persistent by commit span alone", time.Minute, 6, 6, true, "under a minute"},
		// The commit-span threshold is strict >10: exactly 10 stays recent, 11 trips.
		{"span at threshold (10) stays recent", time.Minute, 5, 5, false, "under a minute"},
		{"span just over threshold (11) trips", time.Minute, 6, 5, true, "under a minute"},
		{"unknown age, small span, stays recent", 0, 1, 1, false, "an unknown duration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := SyncFailure{Age: tc.age, Ahead: tc.ahead, Behind: tc.behind}.escalationLine()
			gotIncident := strings.Contains(line, "INCIDENT")
			if gotIncident != tc.wantIncident {
				t.Errorf("escalation incident=%v, want %v: %q", gotIncident, tc.wantIncident, line)
			}
			if !strings.Contains(line, tc.wantAge) {
				t.Errorf("escalation age phrase missing %q: %q", tc.wantAge, line)
			}
		})
	}
}

// SyncFailureError is self-rendering (Error() == the block) and maps to the same
// conflict exit an unresolved reconcile uses, with no second remediation line to
// drift from the block. [LAW:single-enforcer] [LAW:one-source-of-truth]
func TestSyncFailureErrorExitAndRemediation(t *testing.T) {
	err := SyncFailureError{Failure: SyncFailure{
		Class: syncFailureProseHeld, Remote: "origin", Branch: "master",
	}}
	if code := ExitCode(err); code != ExitConflict {
		t.Fatalf("ExitCode = %d, want %d (ExitConflict)", code, ExitConflict)
	}
	if err.Error() != err.Failure.blockString() {
		t.Fatal("SyncFailureError.Error() is not the contract block verbatim")
	}
	if reason := commandErrorReason(err); reason != "sync_divergence" {
		t.Fatalf("reason = %q, want sync_divergence", reason)
	}
	if rem := commandErrorRemediation("sync_divergence"); rem != "" {
		t.Fatalf("sync_divergence remediation = %q, want empty (block is the remediation)", rem)
	}
}

// A remote-schema-ahead block routes to `lit upgrade` and — unlike a divergence —
// frames the state as BLOCKED-until-upgrade, never an age-based "still routine"
// line that would invite the wait-and-retry the epic kills.
func TestSyncFailureBlockRemoteSchemaAhead(t *testing.T) {
	t.Run("producer named", func(t *testing.T) {
		block := SyncFailure{
			Class:               syncFailureRemoteSchemaAhead,
			Remote:              "origin",
			Branch:              "master",
			RemoteSchemaVersion: 7,
			LocalSupportedMax:   4,
			RemoteProducer:      "v9.9.0",
		}.blockString()
		assertContractElements(t, block, "lit upgrade --to v9.9.0")
		for _, want := range []string{"origin/master", "schema version 7", "version 4", "BLOCKED"} {
			if !strings.Contains(block, want) {
				t.Errorf("remote-schema-ahead block missing %q:\n%s", want, block)
			}
		}
		// It must not read as a transient, age-decaying divergence.
		if strings.Contains(block, "INCIDENT") || strings.Contains(block, "still within the window") {
			t.Errorf("remote-schema-ahead block used the divergence-age escalation:\n%s", block)
		}
	})
	t.Run("no producer stamp falls back to generic upgrade", func(t *testing.T) {
		block := SyncFailure{
			Class:               syncFailureRemoteSchemaAhead,
			Remote:              "origin",
			Branch:              "master",
			RemoteSchemaVersion: 7,
			LocalSupportedMax:   4,
		}.blockString()
		assertContractElements(t, block, "lit upgrade")
		if strings.Contains(block, "--to ") {
			t.Errorf("no-producer block should not name a --to target:\n%s", block)
		}
	})
}

// remoteSchemaAheadFailure/asSyncFailure adapt the store's typed remote-ahead
// refusal into the one contract error, exiting ExitConflict — so every surface
// renders the identical block from the same store error. A non-matching error
// passes through asSyncFailure untouched. [LAW:single-enforcer]
func TestRemoteSchemaAheadFailureMapping(t *testing.T) {
	storeErr := &store.RemoteSchemaAheadError{
		Remote: "origin", Branch: "master",
		RemoteVersion: 7, BinarySupportedMax: 4, RemoteProducerVersion: "v9.9.0",
	}
	failure, ok := remoteSchemaAheadFailure(storeErr)
	if !ok {
		t.Fatal("remoteSchemaAheadFailure did not recognize the store error")
	}
	if failure.Class != syncFailureRemoteSchemaAhead || failure.RemoteSchemaVersion != 7 ||
		failure.LocalSupportedMax != 4 || failure.RemoteProducer != "v9.9.0" {
		t.Fatalf("mapped failure = %+v", failure)
	}
	converted := asSyncFailure(storeErr)
	var syncFailure SyncFailureError
	if !errors.As(converted, &syncFailure) {
		t.Fatalf("asSyncFailure did not return a SyncFailureError: %T", converted)
	}
	if code := ExitCode(converted); code != ExitConflict {
		t.Fatalf("ExitCode = %d, want %d (ExitConflict)", code, ExitConflict)
	}
	// A plain error is returned unchanged.
	plain := errors.New("some other failure")
	if asSyncFailure(plain) != plain {
		t.Fatal("asSyncFailure altered a non-remote-schema-ahead error")
	}
}

func TestAgeFromOldestDivergedUnix(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if got := ageFromOldestDivergedUnix(0, now); got != 0 {
		t.Errorf("zero timestamp age = %v, want 0 (unknown)", got)
	}
	if got := ageFromOldestDivergedUnix(now.Add(time.Hour).Unix(), now); got != 0 {
		t.Errorf("future timestamp age = %v, want 0 (never negative)", got)
	}
	if got := ageFromOldestDivergedUnix(now.Add(-6*time.Hour).Unix(), now); got != 6*time.Hour {
		t.Errorf("age = %v, want 6h", got)
	}
}

// doctor turns a divergence that has festered past the persistence threshold into
// a nonzero exit (the sync-failure contract on stderr), while a fresh divergence
// stays a diagnostic note with a zero exit. Exit codes are a contract.
func TestDoctorDivergenceExit(t *testing.T) {
	diverged := func(age time.Duration, ahead, behind int64) doctorSyncReport {
		return doctorSyncReport{
			Kind: doctorSyncResolved,
			Age:  age,
			Freshness: store.SyncFreshness{
				Remote: "origin", Branch: "master", Synced: true, Ahead: ahead, Behind: behind,
			},
		}
	}

	// Persistent by age → SyncFailureError, ExitConflict.
	if err := doctorDivergenceExit(diverged(3*24*time.Hour, 2, 2)); err == nil {
		t.Fatal("persistent divergence did not produce a doctor exit error")
	} else if code := ExitCode(err); code != ExitConflict {
		t.Fatalf("persistent-divergence exit = %d, want %d", code, ExitConflict)
	}

	// Fresh divergence → no error (stays a diagnostic line, zero exit).
	if err := doctorDivergenceExit(diverged(1*time.Hour, 2, 2)); err != nil {
		t.Fatalf("fresh divergence wrongly exited nonzero: %v", err)
	}

	// A non-diverged (behind-only) freshness never trips the incident exit.
	behindOnly := doctorSyncReport{
		Kind: doctorSyncResolved, Age: 10 * 24 * time.Hour,
		Freshness: store.SyncFreshness{Remote: "origin", Branch: "master", Synced: true, Behind: 3},
	}
	if err := doctorDivergenceExit(behindOnly); err != nil {
		t.Fatalf("non-diverged state wrongly exited nonzero: %v", err)
	}
}
