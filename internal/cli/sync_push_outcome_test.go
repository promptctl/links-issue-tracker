package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestPushOutcomeOf pins the derivation from one performSyncPush completion to
// the marker record — every completion shape the deferred write can see.
func TestPushOutcomeOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		outcome syncPushOutcome
		err     error
		want    pushOutcomeRecord
	}{
		{
			name: "could-not-attempt error records error with no remote",
			err:  errors.New("check remote refs \"origin\": exit status 128"),
			want: pushOutcomeRecord{Decision: pushDecisionError, Reason: "check remote refs \"origin\": exit status 128"},
		},
		{
			name: "push that ran and failed records error with the resolved ref",
			outcome: syncPushOutcome{
				remote: "origin", branch: "master",
				pushErr: errors.New("connection refused"),
			},
			want: pushOutcomeRecord{Decision: pushDecisionError, Reason: "connection refused", Remote: "origin", Branch: "master"},
		},
		{
			name:    "no-remote skip records its own decision, not a failure",
			outcome: syncPushOutcome{skip: syncTargetNoRemote},
			want:    pushOutcomeRecord{Decision: "no_sync_remote"},
		},
		{
			name:    "empty-remote skip records its own decision, not a failure",
			outcome: syncPushOutcome{skip: syncTargetRemoteEmpty, remote: "origin"},
			want:    pushOutcomeRecord{Decision: "remote_empty", Remote: "origin"},
		},
		{
			name:    "landed push records pushed",
			outcome: syncPushOutcome{remote: "origin", branch: "master"},
			want:    pushOutcomeRecord{Decision: pushDecisionPushed, Remote: "origin", Branch: "master"},
		},
		{
			name: "cancellation before the attempt records canceled, not error",
			err:  fmt.Errorf("open sync store: %w", context.Canceled),
			want: pushOutcomeRecord{Decision: pushDecisionCanceled, Reason: "open sync store: context canceled"},
		},
		{
			name: "cancellation mid-push records canceled with the resolved ref",
			outcome: syncPushOutcome{
				remote: "origin", branch: "master",
				pushErr: fmt.Errorf("push: %w", context.Canceled),
			},
			want: pushOutcomeRecord{Decision: pushDecisionCanceled, Reason: "push: context canceled", Remote: "origin", Branch: "master"},
		},
		{
			name: "workspace legitimately busy records its own decision, not error",
			err:  fmt.Errorf("open sync store: %w", store.ErrWorkspaceBusy),
			want: pushOutcomeRecord{Decision: pushDecisionWorkspaceBusy, Reason: "open sync store: workspace busy"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every arm is stamped with the observing binary, not just the
			// failing ones — a stamp present on some decisions only would make
			// an empty ObservedBy mean two different things to a later reader.
			tc.want.ObservedBy = "v1.2.3"
			if got := pushOutcomeOf(tc.outcome, tc.err, "v1.2.3"); got != tc.want {
				t.Fatalf("pushOutcomeOf() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPushOutcomeMarkerRoundtrip pins that what one push attempt records is
// exactly what a later command reads back, with a sane age off the file mtime.
func TestPushOutcomeMarkerRoundtrip(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	want := pushOutcomeRecord{Decision: pushDecisionError, Reason: "connection refused", Remote: "origin", Branch: "master"}
	recordPushOutcome(ws, want)

	rec, age, ok := lastPushOutcome(ws, time.Now())
	if !ok {
		t.Fatal("lastPushOutcome() ok = false after recordPushOutcome")
	}
	if rec != want {
		t.Fatalf("lastPushOutcome() = %+v, want %+v", rec, want)
	}
	if age < 0 || age > time.Minute {
		t.Fatalf("lastPushOutcome() age = %v, want just-written", age)
	}

	// A later, healthier attempt overwrites: the marker is the LAST outcome,
	// not a log — the banner clears the moment a push lands.
	recordPushOutcome(ws, pushOutcomeRecord{Decision: pushDecisionPushed, Remote: "origin", Branch: "master"})
	rec, _, ok = lastPushOutcome(ws, time.Now())
	if !ok || rec.failed() {
		t.Fatalf("after a successful push, lastPushOutcome() = %+v ok=%v, want non-failed", rec, ok)
	}
}

// TestLastPushOutcomeAbsentAndCorrupt pins the two non-happy reads: absence is
// a quiet, distinct state (no attempt has happened yet); corruption is a real
// operational fault that must not silently read as one.
func TestLastPushOutcomeAbsentAndCorrupt(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	if _, _, ok := lastPushOutcome(ws, time.Now()); ok {
		t.Fatal("lastPushOutcome() ok = true on a workspace with no marker")
	}

	if err := os.WriteFile(pushOutcomeMarkerPath(ws), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt marker: %v", err)
	}
	if _, _, ok := lastPushOutcome(ws, time.Now()); ok {
		t.Fatal("lastPushOutcome() ok = true on a corrupt marker")
	}
}

// TestSyncPushFailureLines pins the mutation-side banner's predicate: only a
// recorded FAILED attempt warns — never absence, never a landed push, never
// the healthy skips, never a decision this binary does not know.
func TestSyncPushFailureLines(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		rec         pushOutcomeRecord
		age         time.Duration
		known       bool
		running     string
		wantLines   int
		wantSubstrs []string
		wantAbsent  []string
	}{
		{
			name:      "no marker emits nothing",
			known:     false,
			wantLines: 0,
		},
		{
			name:      "landed push emits nothing",
			rec:       pushOutcomeRecord{Decision: pushDecisionPushed, Remote: "origin", Branch: "master"},
			known:     true,
			wantLines: 0,
		},
		{
			name:      "no-remote skip emits nothing",
			rec:       pushOutcomeRecord{Decision: "no_sync_remote"},
			known:     true,
			wantLines: 0,
		},
		{
			name:      "empty-remote skip emits nothing",
			rec:       pushOutcomeRecord{Decision: "remote_empty", Remote: "origin"},
			known:     true,
			wantLines: 0,
		},
		{
			name:      "unknown decision from a newer binary emits nothing",
			rec:       pushOutcomeRecord{Decision: "held_for_review"},
			known:     true,
			wantLines: 0,
		},
		{
			name:        "failed push warns, names the ref, the reason, and the fix",
			rec:         pushOutcomeRecord{Decision: pushDecisionError, Reason: "connection refused", Remote: "origin", Branch: "master"},
			age:         3 * time.Minute,
			known:       true,
			wantLines:   1,
			wantSubstrs: []string{"sync:", "FAILING", "to origin/master", "connection refused", "lit sync push", "stay on this machine"},
		},
		{
			name:        "could-not-attempt failure warns without a ref",
			rec:         pushOutcomeRecord{Decision: pushDecisionError, Reason: "check remote refs: exit status 128"},
			age:         time.Minute,
			known:       true,
			wantLines:   1,
			wantSubstrs: []string{"FAILING", "check remote refs", "lit sync push"},
		},
		{
			// The 2026-08-25 incident, rendered: a schema refusal recorded by
			// 0.7.0, still being replayed to an operator running 0.9.0 — whose
			// binary has supported that schema for days. The reason text cannot
			// say so (it was frozen at write time), so the line must.
			name: "a verdict from an older binary is dated, not replayed as current",
			rec: pushOutcomeRecord{
				Decision:   pushDecisionError,
				Reason:     "remote origin/master is at schema version 5 but this binary supports only up to 4",
				Remote:     "origin",
				Branch:     "master",
				ObservedBy: "0.7.0",
			},
			age:         8 * 24 * time.Hour,
			known:       true,
			running:     "0.9.0",
			wantLines:   1,
			wantSubstrs: []string{"recorded by lit 0.7.0", "now running 0.9.0", "the binary has changed since this verdict", "supports only up to 4"},
		},
		{
			name: "a verdict from this same binary carries no provenance clause",
			rec: pushOutcomeRecord{
				Decision: pushDecisionError, Reason: "connection refused",
				Remote: "origin", Branch: "master", ObservedBy: "0.9.0",
			},
			age:         time.Minute,
			known:       true,
			running:     "0.9.0",
			wantLines:   1,
			wantSubstrs: []string{"FAILING", "connection refused"},
			wantAbsent:  []string{"recorded by lit", "the binary has changed"},
		},
		{
			// Records written before the stamp existed, and dev builds, name no
			// observer. The comparison cannot be made, so nothing is claimed.
			name: "an unstamped record claims nothing about provenance",
			rec: pushOutcomeRecord{
				Decision: pushDecisionError, Reason: "connection refused",
				Remote: "origin", Branch: "master",
			},
			age:         time.Minute,
			known:       true,
			running:     "0.9.0",
			wantLines:   1,
			wantSubstrs: []string{"FAILING", "connection refused"},
			wantAbsent:  []string{"recorded by lit", "the binary has changed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := syncPushFailureLines(tc.rec, tc.age, tc.known, tc.running)
			if len(lines) != tc.wantLines {
				t.Fatalf("syncPushFailureLines() = %d line(s) %q, want %d", len(lines), lines, tc.wantLines)
			}
			joined := strings.Join(lines, "\n")
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(joined, want) {
					t.Fatalf("syncPushFailureLines() = %q, missing %q", joined, want)
				}
			}
			for _, unwanted := range tc.wantAbsent {
				if strings.Contains(joined, unwanted) {
					t.Fatalf("syncPushFailureLines() = %q, must not contain %q", joined, unwanted)
				}
			}
		})
	}
}

// TestFetchStalenessLinesRefIsData pins the one renderer serving both the
// read-side banner (resolved ref) and the store-free mutation-side banner
// (no ref): the warning text adapts, the condition does not.
func TestFetchStalenessLinesRefIsData(t *testing.T) {
	t.Parallel()
	stale := 25 * time.Hour

	withRef := fetchStalenessLines("origin/master", stale, true)
	if len(withRef) != 1 || !strings.Contains(withRef[0], "fetch from origin/master") {
		t.Fatalf("fetchStalenessLines(ref) = %q, want the ref named", withRef)
	}
	refless := fetchStalenessLines("", stale, true)
	if len(refless) != 1 || strings.Contains(refless[0], " from ") {
		t.Fatalf("fetchStalenessLines(no ref) = %q, want no ref clause", refless)
	}
	if got := fetchStalenessLines("origin/master", 2*time.Hour, true); len(got) != 0 {
		t.Fatalf("fetchStalenessLines(fresh) = %q, want none", got)
	}
	if got := fetchStalenessLines("origin/master", stale, false); len(got) != 0 {
		t.Fatalf("fetchStalenessLines(unknown age) = %q, want none", got)
	}
}

// TestOneLineReason pins the banner-size compression: first line, capped,
// never empty.
func TestOneLineReason(t *testing.T) {
	t.Parallel()
	if got := oneLineReason("line one\nline two"); got != "line one" {
		t.Fatalf("oneLineReason(multiline) = %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := oneLineReason(long); len([]rune(got)) != 161 || !strings.HasSuffix(got, "…") {
		t.Fatalf("oneLineReason(long) = %d runes %q, want 160+ellipsis", len([]rune(got)), got)
	}
	if got := oneLineReason("  \n"); got != "(no reason recorded)" {
		t.Fatalf("oneLineReason(blank) = %q", got)
	}
}

// TestPrintMutationSyncStalenessWarning pins the store-free printer end to
// end over real marker files: a failing workspace warns on a mutating
// command's writer; a healthy one stays silent.
func TestPrintMutationSyncStalenessWarning(t *testing.T) {
	t.Parallel()
	now := time.Now()

	t.Run("failed last push warns", func(t *testing.T) {
		ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
		recordPushOutcome(ws, pushOutcomeRecord{Decision: pushDecisionError, Reason: "connection refused", Remote: "origin", Branch: "master"})
		var out bytes.Buffer
		printMutationSyncStalenessWarning(&out, ws, now)
		if !strings.Contains(out.String(), "FAILING") || !strings.Contains(out.String(), "connection refused") {
			t.Fatalf("printMutationSyncStalenessWarning() = %q, want the failure surfaced", out.String())
		}
	})

	t.Run("healthy workspace prints nothing", func(t *testing.T) {
		ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
		recordPushOutcome(ws, pushOutcomeRecord{Decision: pushDecisionPushed, Remote: "origin", Branch: "master"})
		if err := markFetchSuccess(ws); err != nil {
			t.Fatalf("markFetchSuccess: %v", err)
		}
		var out bytes.Buffer
		printMutationSyncStalenessWarning(&out, ws, now)
		if out.Len() != 0 {
			t.Fatalf("printMutationSyncStalenessWarning() = %q, want silence", out.String())
		}
	})

	t.Run("stale fetch warns even with pushes healthy", func(t *testing.T) {
		ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
		recordPushOutcome(ws, pushOutcomeRecord{Decision: pushDecisionPushed, Remote: "origin", Branch: "master"})
		if err := markFetchSuccess(ws); err != nil {
			t.Fatalf("markFetchSuccess: %v", err)
		}
		var out bytes.Buffer
		printMutationSyncStalenessWarning(&out, ws, now.Add(25*time.Hour))
		if !strings.Contains(out.String(), "last successful fetch") {
			t.Fatalf("printMutationSyncStalenessWarning() = %q, want the stale-fetch warning", out.String())
		}
	})
}

// TestDoctorPushHealthDatesTheReplayedVerdict pins doctor's half of the
// stale-verdict fix. The recorded Reason is a sentence frozen at write time and
// printed verbatim; the incident was an operator reading "this binary supports
// only up to 4" off a 0.7.0 record while running a binary that had supported
// schema 5 for six days, and concluding the workspace was broken. Doctor cannot
// re-test the push — it is a read-only diagnostic and re-testing means pushing —
// so what it owes the reader is the date on the verdict it is replaying, and the
// one command that produces a current one.
func TestDoctorPushHealthDatesTheReplayedVerdict(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	recordPushOutcome(ws, pushOutcomeRecord{
		Decision:   pushDecisionError,
		Reason:     "remote origin/master is at schema version 5 but this binary supports only up to 4",
		Remote:     "origin",
		Branch:     "master",
		ObservedBy: "0.7.0",
	})

	var out bytes.Buffer
	if err := printPushOutcomeHealth(&out, ws, time.Now(), "0.9.0"); err != nil {
		t.Fatalf("printPushOutcomeHealth: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"FAILED",
		"recorded by lit 0.7.0",
		"now running 0.9.0",
		"the binary has changed since this verdict",
		"supports only up to 4",
		"run 'lit sync push' to re-test",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor push-health line missing %q:\n%s", want, got)
		}
	}
}

// TestRecordedPushFailureIsNotALatch pins the ticket's first claim as behavior:
// a recorded failure is an observation of one attempt, not a state the workspace
// gets stuck in. The next attempt overwrites it, and a success clears the warning
// — nothing consults the stored verdict to decide whether to try again, so a
// precondition that has since cleared is discovered by the next push rather than
// re-asserted from the record.
func TestRecordedPushFailureIsNotALatch(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	recordPushOutcome(ws, pushOutcomeOf(syncPushOutcome{
		remote: "origin", branch: "master",
		pushErr: errors.New("remote origin/master is at schema version 5 but this binary supports only up to 4"),
	}, nil, "0.7.0"))

	rec, age, known := lastPushOutcome(ws, time.Now())
	if lines := syncPushFailureLines(rec, age, known, "0.9.0"); len(lines) != 1 {
		t.Fatalf("a recorded failure did not warn: %q", lines)
	}

	// The condition clears and the next attempt lands.
	recordPushOutcome(ws, pushOutcomeOf(syncPushOutcome{
		remote: "origin", branch: "master", skip: syncTargetReady,
	}, nil, "0.9.0"))

	rec, age, known = lastPushOutcome(ws, time.Now())
	if rec.Decision != pushDecisionPushed {
		t.Fatalf("the later success did not replace the recorded failure: %+v", rec)
	}
	if lines := syncPushFailureLines(rec, age, known, "0.9.0"); len(lines) != 0 {
		t.Fatalf("the warning outlived the failure it described: %q", lines)
	}
}
