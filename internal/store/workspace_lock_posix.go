//go:build !windows

package store

import (
	"errors"
	"os"
	"syscall"
)

// POSIX implementation of the workspace-lock primitive. flock(2) gives the
// kernel-enforced shared/exclusive semantics the workspace-lock contract
// relies on: multiple LOCK_SH holders coexist on independent FDs, a LOCK_EX
// holder blocks every other holder, and LOCK_NB makes contention observable
// (EWOULDBLOCK) instead of latent.
//
// [LAW:locality-or-seam] Lives behind the tryLockFile / unlockFile seam so
// adding a non-POSIX platform (workspace_lock_windows.go) does not edit this
// file or its callers.

func tryLockFile(file *os.File, exclusive bool) error {
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	err := syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLockWouldBlock
	}
	return err
}

func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
