package process

import (
	"os"
	"os/exec"
	"testing"
)

// A running process is alive, and one that exited and was reaped is not.
func TestAliveIsWhetherAProcessHoldsThePid(t *testing.T) {
	t.Parallel()
	if !Alive(os.Getpid()) {
		t.Fatal("Alive(this process) = false; want true")
	}
	child := exec.Command(os.Args[0], "-test.run=^$")
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("run the test binary: %v\n%s", err, out)
	}
	if Alive(child.Process.Pid) {
		t.Fatalf("Alive(%d), a child that exited and was reaped, = true; want false", child.Process.Pid)
	}
}
