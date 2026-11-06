// Package dirlist reads a directory the program owns, refusing anything at
// that path that is not a directory on every platform.
package dirlist

import "io/fs"

// Read is os.ReadDir that refuses a non-directory at path on every platform
// with the *fs.PathError wrapping syscall.ENOTDIR that POSIX returns, so
// callers classify it the same everywhere. Like os.ReadDir, entries are
// sorted by name and a partial read returns what was read beside the error.
func Read(path string) ([]fs.DirEntry, error) {
	return readDir(path)
}
