// Package pointerstoreintovar reads the clock through a package variable a
// test overwrites through a pointer.
package pointerstoreintovar

import "time"

var clock = time.Now

// Stamp reads clock.
func Stamp() time.Time { return clock() }
