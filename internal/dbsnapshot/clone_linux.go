//go:build linux

package dbsnapshot

import (
	"context"
	"os"

	"golang.org/x/sys/unix"
)

// cloneTree on Linux walks the source and attempts FICLONE per file
// (Btrfs/XFS CoW), falling back to plain copy when FICLONE is rejected by the
// filesystem.
//
// [LAW:dataflow-not-control-flow] Platform variability lives in which file Go
// links in (a value), not in a runtime branch inside one function.
func cloneTree(ctx context.Context, src, dst string) error {
	return walkAndCopy(ctx, src, dst, ficloneOrCopy)
}

func ficloneOrCopy(ctx context.Context, src, dst string) error {
	srcF, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcF.Close()
	info, err := srcF.Stat()
	if err != nil {
		return err
	}
	dstF, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	// Close errors are part of the copy's outcome (write-back-at-close
	// failure means a truncated file), same as plainFileCopy.
	// [LAW:no-silent-failure]
	if err := unix.IoctlFileClone(int(dstF.Fd()), int(srcF.Fd())); err == nil {
		// OpenFile's mode is filtered by umask; Chmod forces exact source perms.
		if err := dstF.Chmod(info.Mode().Perm()); err != nil {
			_ = dstF.Close()
			return err
		}
		return dstF.Close()
	}
	if err := copyWithContext(ctx, dstF, srcF); err != nil {
		_ = dstF.Close()
		return err
	}
	if err := dstF.Chmod(info.Mode().Perm()); err != nil {
		_ = dstF.Close()
		return err
	}
	return dstF.Close()
}
