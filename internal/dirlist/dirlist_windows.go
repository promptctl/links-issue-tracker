package dirlist

import (
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"
)

// readDir checks the kind before the read because Windows' directory read
// cannot be trusted with it: os/dir_windows.go takes the ERROR_FILE_NOT_FOUND
// its listing call returns on a file handle for "no more entries", so a
// regular file reads as an empty directory. One handle answers both, so the
// kind checked is the kind listed.
func readDir(path string) ([]fs.DirEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	entries, err := list(f, path)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return entries, err
}

func list(f *os.File, path string) ([]fs.DirEntry, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: path, Err: syscall.ENOTDIR}
	}
	entries, err := f.ReadDir(-1)
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}
