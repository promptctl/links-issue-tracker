// Package spawnindirect spawns through one function that a static call
// hands git and a dynamic call hands the test binary.
package spawnindirect

import (
	"os"
	"os/exec"
)

// Main is the CLI.
func Main() {}

func spawn(program string) error { return exec.Command(program).Run() }

func apply(f func(string) error, arg string) error { return f(arg) }

// Status runs git.
func Status() error { return spawn("git") }

// Rerun runs the test binary, through a function value.
func Rerun() error { return apply(spawn, os.Args[0]) }
