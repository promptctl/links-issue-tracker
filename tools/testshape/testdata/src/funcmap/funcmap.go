// Package funcmap dispatches through a map of funcs, one of which runs git.
package funcmap

import "os/exec"

var formats = map[string]func() error{
	"plain": func() error { return nil },
	"git":   func() error { return exec.Command("git", "status").Run() },
}

// Format runs the named entry.
func Format(name string) error { return formats[name]() }
