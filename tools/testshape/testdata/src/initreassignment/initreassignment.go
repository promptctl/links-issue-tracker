// Package initreassignment's variable starts as a no-op, and an init
// function stores the filesystem into it: both are its initializer's.
package initreassignment

import "os"

var hook = func() {}

func init() {
	hook = func() { _, _ = os.ReadFile("x") }
}

// Run runs hook.
func Run() { hook() }
