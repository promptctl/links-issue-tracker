package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// openFiles lists the paths of the files this process holds open, read from
// /proc/self/fd.
func openFiles(t *testing.T) []string {
	t.Helper()
	const fdDir = "/proc/self/fd"
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		t.Fatalf("list %s: %v", fdDir, err)
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		p, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		switch {
		case err == nil:
			paths = append(paths, p)
		case errors.Is(err, fs.ErrNotExist):
			// An fd closed between ReadDir and Readlink (the ReadDir's own
			// among them) is no longer open, so it has no path to report.
		default:
			t.Fatalf("read %s: %v", e.Name(), err)
		}
	}
	return paths
}
