//go:build windows

package cli

import (
	"fmt"
	"io"
)

// execIntoBinary on Windows prints a one-line instruction and returns nil —
// Windows does not permit renaming a running .exe in the running process's
// own location atomically the way POSIX rename-over-running-file works, and
// exec semantics differ. The atomic rename has already happened by the time
// this function is called, so re-invoking lit lands the user on the prior
// binary.
func execIntoBinary(path string, argv []string, env []string, w io.Writer) error {
	fmt.Fprintf(w, "downgrade complete. re-run `lit version` to confirm.\n")
	return nil
}
