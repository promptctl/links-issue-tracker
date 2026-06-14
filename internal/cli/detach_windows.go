//go:build windows

package cli

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detachSysProcAttr is the Windows peer of the POSIX detach seam. Windows has
// no session concept; a process started without inheriting the console and
// without being waited on already survives the parent's exit, so no special
// attributes are required. [LAW:locality-or-seam]
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// stillActiveExitCode is the value GetExitCodeProcess reports while a process is
// still running (Win32 STILL_ACTIVE); any other value means it has exited.
const stillActiveExitCode = 259

// processAlive reports whether pid names a live process by querying its exit
// state. lit has no Windows build today — the embedded Dolt engine does not
// compile for Windows — but the probe is genuinely correct, not an always-alive
// placeholder, so wait-for-parent behaves if that ever changes. It uses the same
// golang.org/x/sys/windows surface workspace_lock_windows.go already relies on.
func processAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// The process cannot be opened: it has exited or never existed.
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		// State indeterminate — report alive so the mirror waits rather than
		// racing a possibly-live engine. [LAW:no-ambient-temporal-coupling]
		return true
	}
	return code == stillActiveExitCode
}
