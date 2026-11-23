// Package programfromacall names its spawned program by a function's result,
// which no constant names where it enters the spawn.
package programfromacall

import (
	"os/exec"
	"testing"
)

func gitName() string { return "git" }

func TestAProgramNamedByACallIsUnresolved(t *testing.T) {
	_ = exec.Command(gitName(), "version").Run()
}
