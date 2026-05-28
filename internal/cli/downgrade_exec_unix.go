//go:build !windows

package cli

import (
	"io"

	"golang.org/x/sys/unix"
)

// execIntoBinary replaces the current process image with path on Unix.
// On success this call does not return; on failure (e.g. ENOENT, EACCES) it
// returns the error so the caller can surface it.
//
// [LAW:single-enforcer] Process-replacement lives in exactly one place; the
// platform variant on windows prints an instruction instead.
func execIntoBinary(path string, argv []string, env []string, _ io.Writer) error {
	return unix.Exec(path, argv, env)
}
