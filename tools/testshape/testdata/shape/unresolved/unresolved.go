// Package unresolved runs a program named by the environment.
package unresolved

import (
	"os"
	"os/exec"
)

func RunTool() error { return exec.Command(os.Getenv("TOOL")).Run() }
