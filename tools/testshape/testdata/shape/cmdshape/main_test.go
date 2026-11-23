package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("CMDSHAPE_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRunsItself(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runProgram(t, self, "status")
}

func TestRunsGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	runProgram(t, git, "status")
}

func runProgram(t *testing.T, path string, args ...string) {
	t.Helper()
	_ = exec.Command(path, args...).Run()
}
