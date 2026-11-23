// Package interfacevarininit reads the clock through an interface-typed
// package variable its init function builds.
package interfacevarininit

import "time"

// Clock tells the time.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

var clock Clock

func init() { clock = realClock{} }

// Stamp reads clock.
func Stamp() time.Time { return clock.Now() }
