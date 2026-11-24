// Package buildsgo builds a CLI with the go tool and runs a program named
// at run time, the way a test that builds its own real worker does.
package buildsgo

import (
	"os"
	"os/exec"
)

// Build builds the CLI into path.
func Build(path string) error { return exec.Command("go", "build", "-o", path, ".").Run() }

// RunTool runs the program the environment names.
func RunTool() error { return exec.Command(os.Getenv("TOOL")).Run() }
