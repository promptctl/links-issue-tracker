//go:build windows

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Windows implementation of the workspace-lock primitive. LockFileEx /
// UnlockFileEx are the Win32 analogues of POSIX flock(2): shared and
// exclusive byte-range locks enforced by the kernel across processes.
// We lock the entire address space (low=0xFFFFFFFF, high=0xFFFFFFFF) on
// the lock file so the semantics match flock's whole-file model.
//
// LOCKFILE_FAIL_IMMEDIATELY makes contention observable instead of latent —
// the call returns ERROR_LOCK_VIOLATION rather than blocking, matching the
// LOCK_NB shape the platform-neutral acquisition loop expects.
//
// [LAW:locality-or-seam] Lives behind the same tryLockFile / unlockFile seam
// as the POSIX implementation; no callsite branches on platform.

const (
	lockfileBytesLockLow  uint32 = 0xFFFFFFFF
	lockfileBytesLockHigh uint32 = 0xFFFFFFFF
)

func tryLockFile(file *os.File, exclusive bool) error {
	var flags uint32 = windows.LOCKFILE_FAIL_IMMEDIATELY
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, lockfileBytesLockLow, lockfileBytesLockHigh, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLockWouldBlock
	}
	// LOCKFILE_FAIL_IMMEDIATELY makes LockFileEx synchronous: it either
	// acquires the byte range immediately or returns ERROR_LOCK_VIOLATION.
	// ERROR_IO_PENDING would only be possible without FAIL_IMMEDIATELY (the
	// kernel queued an async lock backed by this Overlapped, which the caller
	// must then wait/cancel). Surfacing it as a real error here keeps the
	// invariant honest — a pending lock must never be silently abandoned.
	return err
}

func unlockFile(file *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, lockfileBytesLockLow, lockfileBytesLockHigh, ol)
}
