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
				status: "ok", remote: "origin", branch: "master",
				pushErr: errors.New("connection refused"),
			},
			want: pushOutcomeRecord{Decision: pushDecisionError, Reason: "connection refused", Remote: "origin", Branch: "master"},
		},
		{
			name:    "no-remote skip records its own decision, not a failure",
			outcome: syncPushOutcome{status: "skipped", reason: "no_sync_remote"},
			want:    pushOutcomeRecord{Decision: "no_sync_remote"},
		},
		{
			name:    "empty-remote skip records its own decision, not a failure",
			outcome: syncPushOutcome{status: "skipped", reason: "remote_empty", remote: "origin"},
			want:    pushOutcomeRecord{Decision: "remote_empty", Remote: "origin"},
		},
		{
			name:    "landed push records pushed",
			outcome: syncPushOutcome{status: "ok", remote: "origin", branch: "master"},
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
				status: "ok", remote: "origin", branch: "master",
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
			if got := pushOutcomeOf(tc.outcome, tc.err); got != tc.want {
				t.Fatalf("pushOutcomeOf() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPushOutcomeMarkerRoundtrip pins that what one push attempt records is
// exactly what a later command reads back, with a sane age off the file mtime.
func TestPushOutcomeMarkerRoundtrip(t *testing.T) {
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
	cases := []struct {
		name        string
		rec         pushOutcomeRecord
		age         time.Duration
		known       bool
		wantLines   int
		wantSubstrs []string
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := syncPushFailureLines(tc.rec, tc.age, tc.known)
			if len(lines) != tc.wantLines {
				t.Fatalf("syncPushFailureLines() = %d line(s) %q, want %d", len(lines), lines, tc.wantLines)
			}
			joined := strings.Join(lines, "\n")
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(joined, want) {
					t.Fatalf("syncPushFailureLines() = %q, missing %q", joined, want)
				}
			}
		})
	}
}

// TestFetchStalenessLinesRefIsData pins the one renderer serving both the
// read-side banner (resolved ref) and the store-free mutation-side banner
// (no ref): the warning text adapts, the condition does not.
func TestFetchStalenessLinesRefIsData(t *testing.T) {
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
