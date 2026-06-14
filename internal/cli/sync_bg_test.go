package cli

import (
	"os/exec"
	"testing"
	"time"
)

// TestWaitForProcessExitReturnsImmediatelyForNoParent pins the sentinel: a
// non-positive PID means "no parent to wait for", so the mirror proceeds at
// once rather than polling out the full timeout.
func TestWaitForProcessExitReturnsImmediatelyForNoParent(t *testing.T) {
	done := make(chan struct{})
	go func() {
		waitForProcessExit(0, time.Second, time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForProcessExit(0) must return immediately, not poll the timeout")
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

	returned := make(chan struct{})
	go func() {
		waitForProcessExit(pid, 5*time.Second, 10*time.Millisecond)
		close(returned)
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
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("waitForProcessExit did not return after the process exited")
	}
}
