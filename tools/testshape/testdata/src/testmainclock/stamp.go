// Package testmainclock reads the clock through a package variable its
// TestMain replaces.
package testmainclock

import "time"

var now = time.Now

// Stamp is now.
func Stamp() time.Time { return now() }
