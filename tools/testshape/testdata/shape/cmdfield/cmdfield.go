// Package cmdfield runs git by storing its path into an exec.Cmd.
package cmdfield

import "os/exec"

// Status runs git status.
func Status() error {
	cmd := &exec.Cmd{Path: "/usr/bin/git", Args: []string{"git", "status"}}
	return cmd.Run()
}
