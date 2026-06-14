package cli

import (
	"os/exec"
	"testing"
	"time"
)

// TestWaitForProcessExitReturnsImmediatelyForNoParent pins the sentinel: a
// non-positive PID means "no parent to wait for", so the mirror proceeds at
// once (true) rather than polling out the full timeout.
func TestWaitForProcessExitReturnsImmediatelyForNoParent(t *testing.T) {
	done := make(chan bool, 1)
	go func() {
		done <- waitForProcessExit(0, time.Second, time.Millisecond)
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("waitForProcessExit(0) must report success (no parent to wait for)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForProcessExit(0) must return immediately, not poll the timeout")
	}
}

// TestWaitForProcessExitReportsTimeout pins the load-bearing half of the race
// fix: when the process outlives the wait, the result is false so the caller
// aborts instead of opening the store as if the parent had exited.
func TestWaitForProcessExitReportsTimeout(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	if waitForProcessExit(cmd.Process.Pid, 100*time.Millisecond, 10*time.Millisecond) {
		t.Fatal("waitForProcessExit must report false when the process outlives the timeout")
	}
}

// TestWaitForProcessExitBlocksUntilProcessExits is the load-bearing ordering
// proof: the mirror must not open its engine while the spawning command is
// still alive. The wait stays blocked for a live process and returns promptly
// once it exits — the kernel's process-liveness signal owns the ordering, not a
// fixed sleep.
func TestWaitForProcessExitBlocksUntilProcessExits(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	pid := cmd.Process.Pid

	returned := make(chan bool, 1)
	go func() {
		returned <- waitForProcessExit(pid, 5*time.Second, 10*time.Millisecond)
	}()

	// While the process is alive the wait must not return.
	select {
	case <-returned:
		t.Fatal("waitForProcessExit returned while the process was still alive")
	case <-time.After(150 * time.Millisecond):
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper process: %v", err)
	}
	_, _ = cmd.Process.Wait()

	select {
	case ok := <-returned:
		if !ok {
			t.Fatal("waitForProcessExit must report success once the process exits")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitForProcessExit did not return after the process exited")
	}
}
