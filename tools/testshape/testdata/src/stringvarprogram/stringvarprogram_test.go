// Package stringvarprogram names its spawned program by a package variable
// whose initializer is "git".
package stringvarprogram

import (
	"os/exec"
	"testing"
)

var tool = "git"

func TestGitNamedByAPackageVariableIsGit(t *testing.T) {
	_ = exec.Command(tool, "version").Run()
}
