package workspace

import (
	"errors"
	"fmt"
	"os"
)

// Initialized answers "has `lit init` run for this workspace": whether a
// directory stands at its DatabasePath, the Dolt root.
//
// The root alone decides it. A database is built beside the root and renamed
// into place whole, and every lock and staging path lit takes before a database
// exists sits beside the root, never inside it, so a root exists only where a
// database was installed. What lies below a present root is the store's to
// judge: an absent tree there is a damaged workspace, not an uninitialized one,
// and sending its owner to `lit init` would be the wrong remediation.
//
// [LAW:one-source-of-truth] Every open of a workspace, read or write, and the
// cross-repository scan ask this one function, so no two of them can give
// different answers about one workspace.
//
// [LAW:no-silent-failure] Only an absent path means "not initialized". Anything
// else standing there, and every other stat failure (EACCES, EIO, ELOOP, …), is
// an error naming it: a workspace in that state is neither initialized nor
// fresh, and guessing either way would send its owner the wrong direction.
func Initialized(databasePath string) (bool, error) {
	info, err := os.Stat(databasePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check whether the lit workspace is initialized: %w", err)
	case !info.IsDir():
		return false, fmt.Errorf("check whether the lit workspace is initialized: %s is not a directory", databasePath)
	}
	return true, nil
}
