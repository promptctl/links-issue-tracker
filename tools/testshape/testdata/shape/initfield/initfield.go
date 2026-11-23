// Package initfield builds its configuration during initialization and reads
// a function from one of its fields.
package initfield

import "os"

type config struct{ mkdir func(string) error }

var cfg = config{mkdir: mkdirAll}

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func Prepare(dir string) error { return cfg.mkdir(dir) }
