package builtcli

import (
	"os"
	"testing"
)

var built string

func TestMain(m *testing.M) {
	built = os.Getenv("BUILT_CLI")
	os.Exit(m.Run())
}

func TestRunsTheBuiltCLI(t *testing.T) { _ = Run(built) }
