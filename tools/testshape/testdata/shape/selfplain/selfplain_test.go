// Package selfplain runs its own test binary, whose TestMain never reaches
// the CLI.
package selfplain

import (
	"os"
	"os/exec"
	"testing"
)

func TestRerun(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	_ = cmd.Run()
}
