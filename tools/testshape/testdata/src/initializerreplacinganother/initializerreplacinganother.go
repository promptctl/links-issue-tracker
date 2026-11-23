// Package initializerreplacinganother has a variable whose value, when run,
// stores a no-op into another variable whose initializer opens a file.
package initializerreplacinganother

import "os"

var open = func() { _, _ = os.Open("x") }

var disarm = func() { open = func() {} }

// Run disarms open, then runs it.
func Run() { disarm(); open() }
