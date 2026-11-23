// Package programconstant names its spawned programs every way a program can
// be named.
package programconstant

import (
	"os/exec"
	"testing"
)

const tool = "git"

func TestGitNamedByAConstantIsGit(t *testing.T) {
	_ = exec.Command(tool, "version").Run()
}

func TestAPathEndingInGitIsGit(t *testing.T) {
	_ = exec.Command("/usr/bin/git", "version").Run()
}

func TestGitLookedUpIsGit(t *testing.T) {
	path, _ := exec.LookPath("git")
	_ = exec.Command(path, "version").Run()
}

func TestACmdBuiltAsAValueSpawnsItsPath(t *testing.T) {
	_ = (&exec.Cmd{Path: "/usr/bin/git"}).Run()
}

func TestAnyOtherProgramIsAnotherProcess(t *testing.T) {
	_ = exec.Command("make").Run()
}

// A local holding the spawner is the spawner: the call is static.
func TestASpawnerInALocalIsStillGit(t *testing.T) {
	start := exec.Command
	_ = start("git").Run()
}

func launch(start func(string, ...string) *exec.Cmd) error { return start("git").Run() }

// Handed on as a value, the spawner is called with a program no one
// resolves.
func TestASpawnerHandedOnAsAValueIsAnotherProcess(t *testing.T) {
	_ = launch(exec.Command)
}
