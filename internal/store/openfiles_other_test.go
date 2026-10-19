//go:build !linux && !darwin

package store

import "testing"

// openFiles has no portable implementation here. On Windows the same contract
// is asserted by every test whose TempDir holds a store or a file:// remote:
// removing a directory fails while any file in it is open.
func openFiles(t *testing.T) []string {
	t.Helper()
	t.Skip("listing this process's open files needs /proc/self/fd or fcntl(F_GETPATH)")
	return nil
}
