package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
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
				remote: remoteName(t, "origin"), branch: "master",
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
			outcome: syncPushOutcome{skip: syncTargetRemoteEmpty, remote: remoteName(t, "origin")},
			want:    pushOutcomeRecord{Decision: "remote_empty", Remote: "origin"},
		},
		{
			name:    "landed push records pushed",
			outcome: syncPushOutcome{remote: remoteName(t, "origin"), branch: "master"},
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
				remote: remoteName(t, "origin"), branch: "master",
				pushErr: fmt.Errorf("push: %w", context.Canceled),
			},
			want: pushOutcomeRecord{Decision: pushDecisionCanceled, Reason: "push: context canceled", Remote: "origin", Branch: "master"},
		},
		{
			name:    "landed push records whether a receive merges here",
			outcome: syncPushOutcome{remote: remoteName(t, "origin"), branch: "master", receiveMerges: true},
			want:    pushOutcomeRecord{Decision: pushDecisionPushed, Remote: "origin", Branch: "master", ReceiveMerges: true},
		},
		{
			name: "push refused over a peer's commits a receive merges records diverged, not error",
			outcome: syncPushOutcome{
				remote: remoteName(t, "origin"), branch: "master", receiveMerges: true,
				pushErr: fmt.Errorf("%w: push remote \"origin\": rejected", storage.ErrPushDiverged),
			},
			want: pushOutcomeRecord{Decision: pushDecisionDiverged, Reason: storage.ErrPushDiverged.Error() + ": push remote \"origin\": rejected", Remote: "origin", Branch: "master", ReceiveMerges: true},
		},
		{
			name: "push refused over a peer's commits nothing merges records error naming the merge",
			outcome: syncPushOutcome{
				remote: remoteName(t, "origin"), branch: "master",
				pushErr: fmt.Errorf("%w: push remote \"origin\": rejected", storage.ErrPushDiverged),
			},
			want: pushOutcomeRecord{Decision: pushDecisionError, Reason: storage.ErrPushDiverged.Error() + ": push remote \"origin\": rejected; no automatic receive runs here to merge the peer's commits: run 'lit sync pull --remote origin', then 'lit sync push --remote origin'", Remote: "origin", Branch: "master"},
		},
		{
			name: "push that failed otherwise records error whether or not a receive merges",
			outcome: syncPushOutcome{
				remote: remoteName(t, "origin"), branch: "master", receiveMerges: true,
				pushErr: errors.New("connection refused"),
			},
			want: pushOutcomeRecord{Decision: pushDecisionError, Reason: "connection refused", Remote: "origin", Branch: "master", ReceiveMerges: true},
		},
		{
			name: "workspace legitimately busy records its own decision, not error",
			err:  fmt.Errorf("open sync store: %w", store.ErrWorkspaceBusy),
			want: pushOutcomeRecord{Decision: pushDecisionWorkspaceBusy, Reason: "open sync store: workspace busy"},
		},
		{
			// The push itself, or its retry after a follow, found the commit
			// lock held: that busy is the push's own and comes back on pushErr.
			name: "workspace busy mid-push records busy with the resolved ref, not error",
			outcome: syncPushOutcome{
				remote: remoteName(t, "origin"), branch: "master",
				pushErr: fmt.Errorf("acquire the commit lock: %w", store.ErrWorkspaceBusy),
			},
			want: pushOutcomeRecord{Decision: pushDecisionWorkspaceBusy, Reason: "acquire the commit lock: workspace busy", Remote: "origin", Branch: "master"},
		},
		{
			// A refusal whose follow-up check met a busy store is still a
			// refusal: the store names the check's failure without wrapping it
			// (joinFollowFailure), so the record says error and the banner warns.
			name: "push refused with a busy follow-up check records error, not busy",
			outcome: syncPushOutcome{
				remote: remoteName(t, "origin"), branch: "master",
				pushErr: fmt.Errorf("push remote \"origin\": rejected (and fetching and following the remote, which could explain the refusal, failed: %v)", store.ErrWorkspaceBusy),
			},
			want: pushOutcomeRecord{Decision: pushDecisionError, Reason: "push remote \"origin\": rejected (and fetching and following the remote, which could explain the refusal, failed: workspace busy)", Remote: "origin", Branch: "master"},
		},
		{
			name: "could-not-attempt the push deadline cut records the verdict and names the deadline",
			err:  cutAtTheDeadline(errors.New("check remote refs \"origin\": signal: killed")),
			want: pushOutcomeRecord{Decision: pushDecisionError, Reason: cutAtTheDeadline(errors.New("check remote refs \"origin\": signal: killed")).Error(), PushDeadlineCut: true},
		},
		{
			// The verdict wraps the push's error, never replaces it, so the
			// ending keeps its class.
			name: "push the deadline cut keeps the class of the error it cut",
			outcome: syncPushOutcome{
				remote: remoteName(t, "origin"), branch: "master",
				pushErr: cutAtTheDeadline(fmt.Errorf("acquire the commit lock: %w", store.ErrWorkspaceBusy)),
			},
			want: pushOutcomeRecord{Decision: pushDecisionWorkspaceBusy, Reason: cutAtTheDeadline(fmt.Errorf("acquire the commit lock: %w", store.ErrWorkspaceBusy)).Error(), Remote: "origin", Branch: "master", PushDeadlineCut: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every arm is stamped with the observing binary, not just the
			// failing ones — a stamp present on some decisions only would make
			// an empty ObservedBy mean two different things to a later reader.
			tc.want.ObservedBy = "v1.2.3"
			got := pushOutcomeOf(tc.outcome, tc.err, "v1.2.3")
			if got != tc.want {
				t.Fatalf("pushOutcomeOf() = %+v, want %+v", got, tc.want)
			}
			if cut := got.eventValue()["push.push_deadline_cut"]; cut != tc.want.PushDeadlineCut {
				t.Fatalf("push.outcomes entry push.push_deadline_cut = %v, want %t", cut, tc.want.PushDeadlineCut)
			}
		})
	}
}

// TestPushOutcomeMarkerRoundtrip pins that what one push attempt records is
// exactly what a later command reads back, with a sane age off the file mtime.
func TestPushOutcomeMarkerRoundtrip(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.CheckoutRoot{})}
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
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.CheckoutRoot{})}
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

// TestFetchStalenessLinesNamesTheFetchedRemote pins the stale-fetch line: it
// names the remote whose fetch it measures, and warns only on a known age at
// or past the threshold.
func TestFetchStalenessLinesNamesTheFetchedRemote(t *testing.T) {
	t.Parallel()
	origin := remoteName(t, "origin")
	stale := &remoteFetch{remote: origin, age: 25 * time.Hour}

	if got := fetchStalenessLines(stale); len(got) != 1 || !strings.Contains(got[0], "fetch from origin was") {
		t.Fatalf("fetchStalenessLines(stale) = %q, want the remote named", got)
	}
	if got := fetchStalenessLines(&remoteFetch{remote: origin, age: 2 * time.Hour}); len(got) != 0 {
		t.Fatalf("fetchStalenessLines(fresh) = %q, want none", got)
	}
	if got := fetchStalenessLines(nil); len(got) != 0 {
		t.Fatalf("fetchStalenessLines(nil) = %q, want none", got)
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
// end over real marker files and a real repository whose one remote is
// origin: a failing workspace warns on a mutating command's writer; a healthy
// one stays silent.
func TestPrintMutationSyncStalenessWarning(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ctx := underInvocation(context.Background())
	workspaceWithOrigin := func(t *testing.T) workspace.Info {
		dir := t.TempDir()
		runGit(t, dir, "init")
		runGit(t, dir, "remote", "add", "origin", filepath.Join(t.TempDir(), "remote.git"))
		return workspace.Info{RootDir: dir, Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), checkoutRoot(t, dir))}
	}

	t.Run("failed last push warns", func(t *testing.T) {
		ws := workspaceWithOrigin(t)
		recordPushOutcome(ws, pushOutcomeRecord{Decision: pushDecisionError, Reason: "connection refused", Remote: "origin", Branch: "master"})
		var out bytes.Buffer
		printMutationSyncStalenessWarning(ctx, testEnv(), &out, ws, now)
		if !strings.Contains(out.String(), "FAILING") || !strings.Contains(out.String(), "connection refused") {
			t.Fatalf("printMutationSyncStalenessWarning() = %q, want the failure surfaced", out.String())
		}
	})

	t.Run("healthy workspace prints nothing", func(t *testing.T) {
		ws := workspaceWithOrigin(t)
		recordPushOutcome(ws, pushOutcomeRecord{Decision: pushDecisionPushed, Remote: "origin", Branch: "master"})
		if err := markFetchSuccess(ws, remoteName(t, "origin")); err != nil {
			t.Fatalf("markFetchSuccess: %v", err)
		}
		var out bytes.Buffer
		printMutationSyncStalenessWarning(ctx, testEnv(), &out, ws, now)
		if out.Len() != 0 {
			t.Fatalf("printMutationSyncStalenessWarning() = %q, want silence", out.String())
		}
	})

	t.Run("stale fetch warns even with pushes healthy", func(t *testing.T) {
		ws := workspaceWithOrigin(t)
		recordPushOutcome(ws, pushOutcomeRecord{Decision: pushDecisionPushed, Remote: "origin", Branch: "master"})
		if err := markFetchSuccess(ws, remoteName(t, "origin")); err != nil {
			t.Fatalf("markFetchSuccess: %v", err)
		}
		var out bytes.Buffer
		printMutationSyncStalenessWarning(ctx, testEnv(), &out, ws, now.Add(25*time.Hour))
		if !strings.Contains(out.String(), "last successful fetch from origin") {
			t.Fatalf("printMutationSyncStalenessWarning() = %q, want the stale-fetch warning naming origin", out.String())
		}
	})
}

// TestDoctorPushHealthDatesTheReplayedVerdict pins that doctor dates a failure
// recorded by a different binary and points at the command that re-tests it.
func TestDoctorPushHealthDatesTheReplayedVerdict(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.CheckoutRoot{})}
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

// TestRecordedPushFailureIsNotALatch pins that a recorded failure is an
// observation of one attempt, not a state the workspace gets stuck in: the next
// attempt overwrites it, and a success clears the warning.
func TestRecordedPushFailureIsNotALatch(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}, Remotes: workspace.NewRemotes(testEnv(), workspace.CheckoutRoot{})}
	recordPushOutcome(ws, pushOutcomeOf(syncPushOutcome{
		remote: remoteName(t, "origin"), branch: "master",
		pushErr: errors.New("remote origin/master is at schema version 5 but this binary supports only up to 4"),
	}, nil, "0.7.0"))

	rec, age, known := lastPushOutcome(ws, time.Now())
	if lines := syncPushFailureLines(rec, age, known, "0.9.0"); len(lines) != 1 {
		t.Fatalf("a recorded failure did not warn: %q", lines)
	}

	// The condition clears and the next attempt lands.
	recordPushOutcome(ws, pushOutcomeOf(syncPushOutcome{
		remote: remoteName(t, "origin"), branch: "master", skip: syncTargetReady,
	}, nil, "0.9.0"))

	rec, age, known = lastPushOutcome(ws, time.Now())
	if rec.Decision != pushDecisionPushed {
		t.Fatalf("the later success did not replace the recorded failure: %+v", rec)
	}
	if lines := syncPushFailureLines(rec, age, known, "0.9.0"); len(lines) != 0 {
		t.Fatalf("the warning outlived the failure it described: %q", lines)
	}
}
