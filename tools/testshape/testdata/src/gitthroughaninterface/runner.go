// Package gitthroughaninterface spawns git from a method called only through
// an interface.
package gitthroughaninterface

import "os/exec"

// Runner is what Drive is handed.
type Runner interface{ Run() error }

type gitRunner struct{ dir string }

func (r gitRunner) Run() error { return exec.Command("git", "-C", r.dir, "status").Run() }

// NewGitRunner builds a git runner and erases its type.
func NewGitRunner(dir string) Runner { return gitRunner{dir} }

// Drive calls the runner it is handed, whatever it is.
func Drive(r Runner) error { return r.Run() }
