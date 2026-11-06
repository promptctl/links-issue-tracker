//go:build !windows

package dirlist

import (
	"io/fs"
	"os"
)

// readDir is os.ReadDir itself: it opens with O_DIRECTORY, so the kernel
// refuses a non-directory with ENOTDIR at open — before a FIFO or device at
// the path could block or act.
func readDir(path string) ([]fs.DirEntry, error) {
	return os.ReadDir(path)
}
