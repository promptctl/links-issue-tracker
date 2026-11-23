// Package modetable opens a store by mode name, through a package-level table
// of openers.
package modetable

import "os"

var openers = map[string]func(string) error{
	"memory": openMemory,
	"disk":   openDisk,
}

func openMemory(string) error { return nil }

func openDisk(dir string) error { return os.MkdirAll(dir, 0o755) }

// Open opens mode's store at dir.
func Open(mode, dir string) error { return openers[mode](dir) }
