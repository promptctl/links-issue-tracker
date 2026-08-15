//go:build darwin

package dbsnapshot

import (
	"context"

	"golang.org/x/sys/unix"
)

// cloneTree on Darwin uses APFS clonefile for a single-syscall CoW clone of
// the whole tree, with walkAndCopy as the fallback when the filesystem
// doesn't support clonefile (or any other failure). The Clonefile call itself
// is uncancelable but metadata-only-fast; ctx governs the fallback walk,
// which is the only path that can run long. Take's entry gate covers the
// pre-canceled case so cancellation semantics don't vary by platform.
//
// [LAW:dataflow-not-control-flow] Platform variability lives in which file Go
// links in (a value), not in a runtime branch inside one function.
func cloneTree(ctx context.Context, src, dst string) error {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err == nil {
		return nil
	}
	return walkAndCopy(ctx, src, dst, plainFileCopy)
}
