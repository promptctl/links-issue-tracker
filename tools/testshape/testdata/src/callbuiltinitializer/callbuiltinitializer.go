// Package callbuiltinitializer builds its function variables by calling a
// constructor in their initializers: the code that computed each value is
// what loading it reaches.
package callbuiltinitializer

import (
	"os/exec"
	"time"
)

func makeClock() func() time.Time {
	return func() time.Time { return time.Now() }
}

var now = makeClock()

// Stamp reads the clock makeClock built.
func Stamp() time.Time { return now() }

func gitHook(name string) func() error {
	return func() error { return exec.Command(name).Run() }
}

// hook runs the program its initializer names, "git".
var hook = gitHook("git")

// Run runs hook.
func Run() error { return hook() }
