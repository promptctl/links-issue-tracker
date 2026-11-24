package cli

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/deadline"
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
			t.Parallel()
			got := cutByPushDeadline(tc.ctx, tc.completionCtx, deadline.Shipped(), tc.err)
			if pushDeadlineCutOf(got) != tc.wantCut {
				t.Fatalf("cutByPushDeadline() = %v, cut = %t; want cut = %t", got, pushDeadlineCutOf(got), tc.wantCut)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("cutByPushDeadline() = %v; it must keep the step's error %v readable", got, tc.err)
			}
			if tc.wantCut && !strings.HasPrefix(got.Error(), pushDeadlineCutExplanation(deadline.Shipped().MirrorPush()).Error()+": ") {
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
	return cutByPushDeadline(endedContext(), context.Background(), deadline.Shipped(), err)
}

// TestMirrorEnvStripsInheritedTraceRefFile pins that the detached mirror never
// inherits the parent command's automation-trace-ref file: otherwise it would
// overwrite the file the parent's caller reads with its own trace path after the
// command returns. The mirror's own trigger/reason are set; nothing else about
// the parent environment is dropped.
func TestMirrorEnvStripsInheritedTraceRefFile(t *testing.T) {
	t.Parallel()
	setRunEnv(t, automationTraceRefFileEnvVar, "/tmp/parent-trace-ref")
	setRunEnv(t, automationTriggerEnvVar, "git-pre-push")
	setRunEnv(t, automationReasonEnvVar, "parent reason")
	setRunEnv(t, "LIT_MIRROR_ENV_CANARY", "keep-me")

	env := mirrorEnv(runEnv(t)).List()

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

// --parent-pid names the spawner as its pid, and nothing else: a worker
// handed a zero, a negative, "none", or no spawner at all is refused, so no
// worker skips its wait by omission, and the pid a spawner is handed on is
// the one it was parsed from.
func TestParseSpawnerTakesOnlyAPositivePid(t *testing.T) {
	t.Parallel()
	got, err := parseSpawner("4242")
	if err != nil || got.flag() != "4242" {
		t.Fatalf("parseSpawner(\"4242\") = %v, %v; want pid 4242", got, err)
	}
	if back, err := parseSpawner(got.flag()); err != nil || back != got {
		t.Errorf("parseSpawner(%v.flag()) = %v, %v; want %v back", got, back, err, got)
	}
	for _, flag := range []string{"", "0", "-1", "none", "pid"} {
		if got, err := parseSpawner(flag); err == nil {
			t.Errorf("parseSpawner(%q) = %v; want it refused", flag, got)
		}
	}
}

// spawnerOf is the spawner --parent-pid=pid names, as the worker parses it.
func spawnerOf(t *testing.T, pid int) spawner {
	t.Helper()
	from, err := parseSpawner(strconv.Itoa(pid))
	if err != nil {
		t.Fatalf("parseSpawner(%d) error = %v", pid, err)
	}
	return from
}

// A worker whose spawner is no longer its parent proceeds at once, waiting
// out nothing.
func TestAWorkerWhoseSpawnerHasExitedWaitsForNone(t *testing.T) {
	t.Parallel()
	calls := 0
	if !spawnerOf(t, 5149).awaitExit(context.Background(), func() int { calls++; return 1 }, time.Second, time.Millisecond) {
		t.Fatal("awaitExit must report success for a spawner that is no longer the parent")
	}
	if calls != 1 {
		t.Fatalf("getppid was polled %d times for a spawner already gone, want once", calls)
	}
}

// TestASpawnedWorkerWaitsUntilReparented is the load-bearing proof: while
// getppid still names the spawning command the wait blocks, and as soon as the
// worker is reparented (getppid changes) it returns true. The injected getppid
// flips after a few polls, standing in for the parent exiting — no real process
// tree, no platform sleep binary.
func TestASpawnedWorkerWaitsUntilReparented(t *testing.T) {
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
	if !spawnerOf(t, parentPID).awaitExit(context.Background(), getppid, time.Second, time.Millisecond) {
		t.Fatal("awaitExit must report success once getppid stops naming the parent")
	}
	if polls < 3 {
		t.Fatalf("expected the wait to poll until reparented, got %d polls", polls)
	}
}

// TestASpawnedWorkerReportsASpawnerThatOutlivesTheWait pins the other half: when
// the parent outlives the wait (getppid never changes), the result is false so
// the caller aborts instead of opening the store.
func TestASpawnedWorkerReportsASpawnerThatOutlivesTheWait(t *testing.T) {
	t.Parallel()
	const parentPID = 5151
	if spawnerOf(t, parentPID).awaitExit(context.Background(), func() int { return parentPID }, 30*time.Millisecond, time.Millisecond) {
		t.Fatal("awaitExit must report false when the parent never exits before the timeout")
	}
}

// TestASpawnedWorkersWaitEndsOnTeardown pins that a done context ends the wait
// immediately (false), long before the deadline: a mirror in its SIGTERM grace
// window must spend it releasing state, not sleeping toward a timeout it will
// be killed before reaching.
func TestASpawnedWorkersWaitEndsOnTeardown(t *testing.T) {
	t.Parallel()
	const parentPID = 5152
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if spawnerOf(t, parentPID).awaitExit(ctx, func() int { return parentPID }, time.Minute, time.Millisecond) {
		t.Fatal("awaitExit must report false when torn down mid-wait")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("teardown took %s to end the wait; it must not sleep toward the deadline", elapsed)
	}
}
