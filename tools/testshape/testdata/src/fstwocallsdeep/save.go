// Package fstwocallsdeep writes a file two calls below its entry point.
package fstwocallsdeep

import (
	"os"
	"path/filepath"
)

// Save writes through write.
func Save(dir string) error { return write(dir) }

func write(dir string) error {
	return os.WriteFile(filepath.Join(dir, "saved"), nil, 0o644)
}
