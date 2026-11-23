// Package nested is its own module, under the path of the module that
// imports it.
package nested

import "os"

// Dial reads a file; the fixture classification charges it net instead, so
// a measure that walked it as the importing module's code would say fs.
func Dial() ([]byte, error) { return os.ReadFile("peer") }
