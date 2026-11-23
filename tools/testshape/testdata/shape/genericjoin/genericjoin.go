// Package genericjoin hands a module type to an outside generic as its type
// argument; the generic calls the type's String through its constraint.
package genericjoin

import (
	"os"

	"example.com/shape/nested/strs"
)

type file string

func (f file) String() string {
	b, _ := os.ReadFile(string(f))
	return string(b)
}

func Names() string { return strs.Join([]file{"a"}) }
