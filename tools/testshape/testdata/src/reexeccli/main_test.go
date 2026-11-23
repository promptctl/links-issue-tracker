package main

import (
	"os"
	"os/exec"
	"testing"
)

// TestMain sends a child it is re-run as to main, the CLI, and runs the
// tests otherwise.
func TestMain(m *testing.M) {
	if os.Getenv("FIXTURE_REEXEC") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestReRunningTheBinaryRunsTheCLI(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "version")
	cmd.Env = append(os.Environ(), "FIXTURE_REEXEC=1")
	_ = cmd.Run()
}
