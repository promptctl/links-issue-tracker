//go:build windows

package cli

import "syscall"

// detachSysProcAttr is the Windows peer of the POSIX detach seam. Windows has
// no session concept; a process started without inheriting the console and
// without being waited on already survives the parent's exit, so no special
// attributes are required. [LAW:locality-or-seam] Wait-for-parent is
// platform-neutral (it watches getppid in sync_bg.go), so this seam carries only
// the spawn attributes. lit has no Windows build today (the embedded Dolt engine
// does not compile for Windows); this exists so the seam type-checks.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
