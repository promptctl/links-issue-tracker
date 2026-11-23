// Package gitwrap runs git through two helpers.
package gitwrap

import "os/exec"

func Status(dir string) ([]byte, error) { return git(dir, "status") }

func git(dir string, args ...string) ([]byte, error) { return run(dir, "git", args...) }

func run(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd.Output()
}
