// Package helperprocess re-runs its test binary to run one test in a child:
// the helper-process idiom, not a CLI.
package helperprocess

import (
	"os"
	"os/exec"
	"testing"
)

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func TestReRunningTheBinaryForOneTestIsAnotherProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelper$")
	cmd.Env = append(os.Environ(), "FIXTURE_HELPER=1")
	_ = cmd.Run()
}

func TestHelper(t *testing.T) {
	if os.Getenv("FIXTURE_HELPER") != "1" {
		t.Skip("run only as a child")
	}
}
