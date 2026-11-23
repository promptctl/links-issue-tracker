// Package initmap builds its openers during initialization and reads them
// through a map.
package initmap

import "os"

type Opener interface{ Open(dir string) error }

type disk struct{}

func (disk) Open(dir string) error { return os.MkdirAll(dir, 0o755) }

var byName = map[string]Opener{}

func init() { byName["disk"] = disk{} }

func Open(name, dir string) error { return byName[name].Open(dir) }
