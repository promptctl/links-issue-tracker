// Package dirlist reads a directory the program owns, refusing anything at
// that path that is not a directory on every platform.
package dirlist

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"
)

// Read is os.ReadDir that refuses a non-directory at path on every platform.
// Windows' readdir reports a regular file as an empty directory, so without
// the check a file where an owned directory belongs would read as no entries.
// The refusal is the *fs.PathError wrapping syscall.ENOTDIR that POSIX
// readdir returns, so callers classify it the same everywhere. The check and
// the read share one handle, so the path cannot be swapped between them.
// Entries are sorted by name, as os.ReadDir sorts them.
func Read(path string) (entries []fs.DirEntry, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: path, Err: syscall.ENOTDIR}
	}
	entries, err = f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}
