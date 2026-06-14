//go:build !windows

package cli

import (
	"errors"
	"syscall"
)

// detachSysProcAttr puts the background mirror in its own session so it
// outlives the command that spawned it. [LAW:no-ambient-temporal-coupling] The
// mirror's lifetime is owned by this explicit detach, not by the parent process
// tree — when the short-lived command exits, the mirror keeps running and
// pushes after the command's embedded Dolt engine has been released.
//
// [LAW:locality-or-seam] Setsid is POSIX-only, so the platform variance lives
// behind this seam (detach_windows.go is the parallel-but-isolated peer); the
// spawn path and the mirror loop never see a build tag.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// processAlive reports whether pid names a live process. Signal 0 delivers
// nothing — it only runs the kernel's existence/permission check, returning
// ESRCH once the process is gone. The mirror polls this to wait out the
// spawning command's engine before opening its own. EPERM means the process
// exists but is owned by another user (not expected for sibling lit processes);
// it is treated as alive so the mirror waits rather than racing the engine.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
