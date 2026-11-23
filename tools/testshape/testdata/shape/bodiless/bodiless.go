// Package bodiless calls a module function that has no Go body.
package bodiless

import _ "unsafe"

//go:linkname nanotime runtime.nanotime
func nanotime() int64

func Now() int64 { return nanotime() }
