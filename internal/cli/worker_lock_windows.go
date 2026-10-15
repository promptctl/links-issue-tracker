//go:build windows

package cli

import "os"

// holdWorkerLock is the Windows peer of the POSIX worker hold. os/exec cannot
// pass an extra file to a child on Windows, so a Windows worker holds nothing,
// and JoinDetachedWorkers is not built here: no Windows caller can ask a
// question this build cannot answer. lit has no Windows build today (see
// detach_windows.go).
func holdWorkerLock(string) ([]*os.File, error) { return nil, nil }

// keepWorkerHoldFromChildren has nothing to keep: a Windows worker holds no lock.
func keepWorkerHoldFromChildren() {}
