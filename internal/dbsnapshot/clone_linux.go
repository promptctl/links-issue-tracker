//go:build linux

package dbsnapshot

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// cloneTree on Linux walks the source and attempts FICLONE per file
// (Btrfs/XFS CoW), falling back to plain copy when FICLONE is rejected by the
// filesystem.
//
// [LAW:dataflow-not-control-flow] Platform variability lives in which file Go
// links in (a value), not in a runtime branch inside one function.
func cloneTree(src, dst string) error {
	return walkAndCopy(src, dst, ficloneOrCopy)
}

func ficloneOrCopy(src, dst string) error {
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
	defer dstF.Close()
	if err := unix.IoctlFileClone(int(dstF.Fd()), int(srcF.Fd())); err == nil {
		return nil
	}
	if _, err := io.Copy(dstF, srcF); err != nil {
		return err
	}
	return nil
}
