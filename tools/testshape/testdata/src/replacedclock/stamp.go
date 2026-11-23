// Package replacedclock reads the clock through a package variable a test
// can replace.
package replacedclock

import "time"

var now = time.Now

// Stamp is now.
func Stamp() time.Time { return now() }
