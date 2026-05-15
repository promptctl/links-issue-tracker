//go:build darwin

package dbsnapshot

import "golang.org/x/sys/unix"

// cloneTree on Darwin uses APFS clonefile for a single-syscall CoW clone of
// the whole tree, with walkAndCopy as the fallback when the filesystem
// doesn't support clonefile (or any other failure).
//
// [LAW:dataflow-not-control-flow] Platform variability lives in which file Go
// links in (a value), not in a runtime branch inside one function.
func cloneTree(src, dst string) error {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err == nil {
		return nil
	}
	return walkAndCopy(src, dst, plainFileCopy)
}
