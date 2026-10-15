package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

// TestSpawnedWorkerPIDsReadsEveryWorkerLogAndGeneration pins where spawn
// records are read from: both workers' logs, the rotated generation as well as
// the current one, and nothing but the spawn records in them — a worker's own
// lines carry a pid too, and a stderr line can start with anything.
func TestSpawnedWorkerPIDsReadsEveryWorkerLogAndGeneration(t *testing.T) {
	storageDir := t.TempDir()
	logs := map[string]string{
		"mirror.log.1": "2026-10-15T12:00:00Z spawned __mirror-bg pid=101 parent_pid=1\n" +
			"2026-10-15T12:00:01Z mirror cycle start (hold budget 20s, push deadline 40s)\n",
		"mirror.log": "2026-10-15T12:01:00Z spawned __mirror-bg pid=102 parent_pid=2\n" +
			"lit: mirror beacon not released (busy); concurrent claims may read this dying mirror as live until process exit\n",
		"receive.log": "2026-10-15T12:02:00Z spawned __receive-bg pid=201 parent_pid=3\n" +
			"2026-10-15T12:02:00Z receive start pid=201 deadline=15s\n" +
			"2026-10-15T12:02:01Z receive end pid=201 mirror_wait=0s elapsed=1s\n",
	}
	for name, body := range logs {
		if err := os.WriteFile(filepath.Join(storageDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got, err := SpawnedWorkerPIDs(storageDir)
	if err != nil {
		t.Fatalf("SpawnedWorkerPIDs() = %v", err)
	}
	slices.Sort(got)
	if want := []int{101, 102, 201}; !slices.Equal(got, want) {
		t.Fatalf("SpawnedWorkerPIDs() = %v, want %v", got, want)
	}
}

// TestSpawnedWorkerPIDsOfAStoreWithNoWorkerLogsIsEmpty pins that a store no
// worker was ever spawned for answers "none", not an error.
func TestSpawnedWorkerPIDsOfAStoreWithNoWorkerLogsIsEmpty(t *testing.T) {
	got, err := SpawnedWorkerPIDs(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("SpawnedWorkerPIDs(empty store) = %v, %v; want none and no error", got, err)
	}
}
