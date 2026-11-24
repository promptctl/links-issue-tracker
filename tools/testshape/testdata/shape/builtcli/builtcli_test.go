package builtcli

import (
	"os"
	"os/exec"
	"testing"
)

var built string

// TestMain builds the CLI apart from the test binary, so the setup never runs
// the CLI, yet a CLI exists for every test to run.
func TestMain(m *testing.M) {
	built = os.Getenv("BUILT_CLI")
	_ = exec.Command("go", "build", "-o", built, ".").Run()
	os.Exit(m.Run())
}

func TestRunsTheBuiltCLI(t *testing.T) { _ = Run(built) }

func TestRunsItAgain(t *testing.T) { _ = Run(built) }
