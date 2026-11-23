// Package initpointer builds its opener during initialization and reads it
// through a pointer.
package initpointer

import "os"

type Opener interface{ Open(dir string) error }

type disk struct{}

func (disk) Open(dir string) error { return os.MkdirAll(dir, 0o755) }

var current *Opener

func init() {
	var o Opener = disk{}
	current = &o
}

func Open(dir string) error { return (*current).Open(dir) }
