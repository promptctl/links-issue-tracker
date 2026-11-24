// Package process answers questions about other processes by pid.
package process

import (
	"errors"
	"os"
	"syscall"
)

// Alive reports whether a process holds pid. os.FindProcess fails for a dead
// pid on Windows; on Unix it always succeeds, and the null signal comes back
// as os.ErrProcessDone once no process holds the pid. Any other answer to the
// null signal — a process of another user's refusing it, among them — is a
// process that exists. A zombie still holds its pid until it is reaped, and a
// pid reused by a new process reads as alive: the answer is about the pid, not
// about whichever process the caller once knew by it.
func Alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer func() { _ = p.Release() }()
	return !errors.Is(p.Signal(syscall.Signal(0)), os.ErrProcessDone)
}
