//go:build windows

package cli

import (
	"os"
	"syscall"
)

// detachSysProcAttr is the Windows peer of the POSIX detach seam. Windows has
// no session concept; a process started without inheriting the console and
// without being waited on already survives the parent's exit, so no special
// attributes are required. [LAW:locality-or-seam]
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// processAlive reports whether pid names a live process. On Windows
// os.FindProcess opens a handle and fails once the process is gone, which is a
// best-effort liveness check sufficient for the mirror's wait-for-parent.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = proc.Release()
	return true
}
