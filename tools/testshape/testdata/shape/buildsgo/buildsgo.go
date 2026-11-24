// Package buildsgo spawns the go tool and runs a program named at run time:
// the way a test that builds its own real worker does, and the way one that
// only asks the go tool something does.
package buildsgo

import (
	"os"
	"os/exec"
)

// Build builds the CLI into path.
func Build(path string) error { return exec.Command("go", "build", "-o", path, ".").Run() }

// CompileTest builds the package's test binary into path.
func CompileTest(path string) error {
	return exec.Command("go", "test", "-c", "-o", path, ".").Run()
}

// List lists the packages: it builds nothing.
func List() error { return exec.Command("go", "list", "-deps", "./...").Run() }

// Test runs the packages' tests, leaving no binary behind.
func Test() error { return exec.Command("go", "test", "-count=1", "./...").Run() }

// Go runs the go subcommand the environment names.
func Go() error { return exec.Command("go", os.Getenv("GO_SUBCOMMAND")).Run() }

// RunTool runs the program the environment names.
func RunTool() error { return exec.Command(os.Getenv("TOOL")).Run() }
