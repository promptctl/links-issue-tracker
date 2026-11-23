// Package anothertestsfake reads a package variable whose initializer touches
// nothing, and one of its tests stores a fake that touches the filesystem.
package anothertestsfake

import "time"

var now = func() time.Time { return time.Time{} }

// Stamp reads now.
func Stamp() time.Time { return now() }
