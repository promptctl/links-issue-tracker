// Package testmainchildstore reads a package variable whose initializer opens
// a file, and its TestMain stores a no-op into it in a child's branch.
package testmainchildstore

import "os"

var hook = func() { _, _ = os.Open("x") }

// Run runs hook.
func Run() { hook() }
