// Package installedhook's hook is stored by a helper its init function
// calls, which names the program.
package installedhook

import "os/exec"

var hook func() error

func install(name string) {
	hook = func() error { return exec.Command(name).Run() }
}

func init() { install("git") }

// Run runs hook.
func Run() error { return hook() }
