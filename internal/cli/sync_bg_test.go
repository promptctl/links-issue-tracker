package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestCutByPushDeadline pins the one verdict every step the push deadline
// bounds takes where it returns — the clone's open, the remote resolution,
// the push (links-storage-3k6u.98l).
func TestCutByPushDeadline(t *testing.T) {
	t.Parallel()
	live := context.Background()
	stepErr := errors.New("check remote refs \"origin\": signal: killed")
	cases := []struct {
		name               string
		ctx, completionCtx context.Context
		err                error
		wantCut            bool
	}{
		{"a step that succeeded is not cut, whatever its deadline did", endedContext(), live, nil, false},
		{"a step that failed on its own while its deadline was live is not cut", live, live, stepErr, false},
		{"a step that failed once its deadline ended is cut", endedContext(), live, stepErr, true},
		{"a step the process tore down is not cut by its deadline", endedContext(), endedContext(), stepErr, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cutByPushDeadline(tc.ctx, tc.completionCtx, tc.err)
			if pushDeadlineCutOf(got) != tc.wantCut {
				t.Fatalf("cutByPushDeadline() = %v, cut = %t; want cut = %t", got, pushDeadlineCutOf(got), tc.wantCut)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("cutByPushDeadline() = %v; it must keep the step's error %v readable", got, tc.err)
			}
			if tc.wantCut && !strings.HasPrefix(got.Error(), pushDeadlineCutExplanation().Error()+": ") {
				t.Fatalf("cut error %q does not lead with the deadline's explanation", got)
			}
		})
	}

}

// endedContext is an operation lifetime that has already ended.
func endedContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// cutAtTheDeadline is err as a step returns it once its push deadline has
// ended under a live completion lifetime: through cutByPushDeadline, the one
// constructor of the verdict.
func cutAtTheDeadline(err error) error {
	return cutByPushDeadline(endedContext(), context.Background(), err)
}

// TestMirrorEnvStripsInheritedTraceRefFile pins that the detached mirror never
// inherits the parent command's automation-trace-ref file: otherwise it would
// overwrite the file the parent's caller reads with its own trace path after the
// command returns. The mirror's own trigger/reason are set; nothing else about
// the parent environment is dropped.
func TestMirrorEnvStripsInheritedTraceRefFile(t *testing.T) {
	t.Setenv(automationTraceRefFileEnvVar, "/tmp/parent-trace-ref")
	t.Setenv(automationTriggerEnvVar, "git-pre-push")
	t.Setenv(automationReasonEnvVar, "parent reason")
	t.Setenv("LIT_MIRROR_ENV_CANARY", "keep-me")

	env := mirrorEnv()

	var trigger, reason, canary string
	for _, kv := range env {
		if strings.HasPrefix(kv, automationTraceRefFileEnvVar+"=") {
			t.Fatalf("mirrorEnv leaked the parent trace-ref file: %q", kv)
		}
		switch {
		case strings.HasPrefix(kv, automationTriggerEnvVar+"="):
			trigger = strings.TrimPrefix(kv, automationTriggerEnvVar+"=")
		case strings.HasPrefix(kv, automationReasonEnvVar+"="):
			reason = strings.TrimPrefix(kv, automationReasonEnvVar+"=")
		case strings.HasPrefix(kv, "LIT_MIRROR_ENV_CANARY="):
			canary = strings.TrimPrefix(kv, "LIT_MIRROR_ENV_CANARY=")
		}
	}
	if trigger != "on-change" {
		t.Fatalf("trigger = %q, want on-change (mirror's own, not the parent's)", trigger)
	}
	if reason == "" || reason == "parent reason" {
		t.Fatalf("reason = %q, want the mirror's own reason", reason)
	}
	if canary != "keep-me" {
		t.Fatalf("mirrorEnv dropped an unrelated parent var: canary = %q", canary)
	}
}

// TestWaitForParentExitReturnsImmediatelyForNoParent pins the sentinel: a
// non-positive parent pid means "no parent to wait for", so the mirror proceeds
// at once (true) without polling.
func TestWaitForParentExitReturnsImmediatelyForNoParent(t *testing.T) {
	t.Parallel()
	calls := 0
	got := waitForParentExit(context.Background(), 0, func() int { calls++; return 4242 }, time.Second, time.Millisecond)
	if !got {
		t.Fatal("waitForParentExit(0, ...) must report success without waiting")
	}
	if calls != 0 {
		t.Fatalf("getppid was polled %d times for a no-parent wait, want 0", calls)
	}
}

// TestWaitForParentExitReturnsWhenReparented is the load-bearing proof: while
// getppid still names the spawning command the wait blocks, and as soon as the
// worker is reparented (getppid changes) it returns true. The injected getppid
// flips after a few polls, standing in for the parent exiting — no real process
// tree, no platform sleep binary.
func TestWaitForParentExitReturnsWhenReparented(t *testing.T) {
	t.Parallel()
	const parentPID = 5150
	polls := 0
	getppid := func() int {
		polls++
		if polls < 3 {
			return parentPID // parent still alive
		}
		return 1 // reparented to init: parent exited
	}
	if !waitForParentExit(context.Background(), parentPID, getppid, time.Second, time.Millisecond) {
		t.Fatal("waitForParentExit must report success once getppid stops naming the parent")
	}
	if polls < 3 {
		t.Fatalf("expected the wait to poll until reparented, got %d polls", polls)
	}
}

// TestWaitForParentExitReportsTimeout pins the other half: when
// the parent outlives the wait (getppid never changes), the result is false so
// the caller aborts instead of opening the store.
func TestWaitForParentExitReportsTimeout(t *testing.T) {
	t.Parallel()
	const parentPID = 5151
	if waitForParentExit(context.Background(), parentPID, func() int { return parentPID }, 30*time.Millisecond, time.Millisecond) {
		t.Fatal("waitForParentExit must report false when the parent never exits before the timeout")
	}
}

// TestWaitForParentExitEndsOnTeardown pins that a done context ends the wait
// immediately (false), long before the deadline: a mirror in its SIGTERM grace
// window must spend it releasing state, not sleeping toward a timeout it will
// be killed before reaching.
func TestWaitForParentExitEndsOnTeardown(t *testing.T) {
	t.Parallel()
	const parentPID = 5152
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if waitForParentExit(ctx, parentPID, func() int { return parentPID }, time.Minute, time.Millisecond) {
		t.Fatal("waitForParentExit must report false when torn down mid-wait")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("teardown took %s to end the wait; it must not sleep toward the deadline", elapsed)
	}
}
