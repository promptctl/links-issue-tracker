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

// processAlive is a best-effort liveness probe for the wait-for-parent. lit has
// no Windows build — the embedded Dolt engine (gozstd / go-mysql-server cgo) does
// not compile for Windows, so this file never executes; it exists only so the
// platform seam type-checks, mirroring workspace_lock_windows.go. os.FindProcess
// on Windows opens a handle that lingers briefly after exit, so this errs toward
// "alive" — the same limitation the store's isCommitLockPIDRunning carries. That
// bias is deliberately the safe one: waitForProcessExit treats a never-exiting
// probe as a timeout, and the mirror aborts on timeout (records a trace) rather
// than racing a live engine. A precise OpenProcess/GetExitCodeProcess probe is
// withheld on purpose: it would be code CI can neither compile nor run.
// [LAW:verifiable-goals]
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = proc.Release()
	return true
}
