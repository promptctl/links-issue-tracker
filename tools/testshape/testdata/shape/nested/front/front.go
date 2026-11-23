// Package front is a module package's direct dependency, which imports deep.
package front

import "example.com/shape/nested/deep"

// Home is deep's.
func Home() string { return deep.Home }
