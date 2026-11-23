// Package inmemory is a store port with an in-memory implementation and an
// on-disk one whose constructor touches the filesystem.
package inmemory

import "os"

type Store interface{ Get(key string) string }

type Memory map[string]string

func (m Memory) Get(key string) string { return m[key] }

type Disk struct{ dir string }

func (d Disk) Get(key string) string {
	b, _ := os.ReadFile(d.dir + "/" + key)
	return string(b)
}

// OpenDisk makes the store's directory.
func OpenDisk(dir string) (Store, error) {
	return Disk{dir}, os.MkdirAll(dir, 0o755)
}
