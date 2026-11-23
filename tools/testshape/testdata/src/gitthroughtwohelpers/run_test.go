// Package gitthroughtwohelpers passes the program it spawns through helpers.
package gitthroughtwohelpers

import (
	"os/exec"
	"testing"
)

func run(t *testing.T, dir, prog string, args ...string) {
	t.Helper()
	if err := runIn(dir, prog, args); err != nil {
		t.Fatal(err)
	}
}

func runIn(dir, prog string, args []string) error {
	cmd := exec.Command(prog, args...)
	cmd.Dir = dir
	return cmd.Run()
}

func TestGitThroughTwoHelpersIsGit(t *testing.T) {
	run(t, "", "git", "status")
}

func TestMakeThroughTwoHelpersIsAnotherProcess(t *testing.T) {
	run(t, "", "make")
}

// The program reaches the spawn as a closure's captured variable.
func TestGitCapturedByAClosureIsGit(t *testing.T) {
	prog := "git"
	spawn := func() error { return exec.Command(prog, "version").Run() }
	_ = spawn()
}
