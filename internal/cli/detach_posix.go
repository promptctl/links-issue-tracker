//go:build !windows

package cli

import "syscall"

// detachSysProcAttr puts the background mirror in its own session so it
// outlives the command that spawned it. [LAW:no-ambient-temporal-coupling] The
// mirror's lifetime is owned by this explicit detach, not by the parent process
// tree — when the short-lived command exits, the mirror keeps running and
// pushes after the command's embedded Dolt engine has been released.
//
// [LAW:locality-or-seam] Setsid is POSIX-only, so the platform variance lives
// behind this seam (detach_windows.go is the parallel-but-isolated peer); the
// spawn path and the mirror loop never see a build tag. Wait-for-parent itself
// is platform-neutral (it watches getppid), so it lives in sync_bg.go.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
