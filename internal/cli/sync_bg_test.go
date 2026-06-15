package cli

import (
	"strings"
	"testing"
	"time"
)

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
	calls := 0
	got := waitForParentExit(0, func() int { calls++; return 4242 }, time.Second, time.Millisecond)
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
	const parentPID = 5150
	polls := 0
	getppid := func() int {
		polls++
		if polls < 3 {
			return parentPID // parent still alive
		}
		return 1 // reparented to init: parent exited
	}
	if !waitForParentExit(parentPID, getppid, time.Second, time.Millisecond) {
		t.Fatal("waitForParentExit must report success once getppid stops naming the parent")
	}
	if polls < 3 {
		t.Fatalf("expected the wait to poll until reparented, got %d polls", polls)
	}
}

// TestWaitForParentExitReportsTimeout pins the other half of the race fix: when
// the parent outlives the wait (getppid never changes), the result is false so
// the caller aborts instead of opening the store.
func TestWaitForParentExitReportsTimeout(t *testing.T) {
	const parentPID = 5151
	if waitForParentExit(parentPID, func() int { return parentPID }, 30*time.Millisecond, time.Millisecond) {
		t.Fatal("waitForParentExit must report false when the parent never exits before the timeout")
	}
}
