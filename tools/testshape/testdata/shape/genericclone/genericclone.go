// Package genericclone hands a module type with a touching method to two
// outside generics, beside a conversion that hands fmt an interface whose
// values reflection may reach: slices.Clone, read to convert none of its
// type arguments' values to an interface, and strs.Keep, which does.
package genericclone

import (
	"fmt"
	"os"
	"slices"

	"example.com/shape/nested/strs"
)

type omission struct{ path string }

// removeFrom is called by nothing but reflection over a runtime omission.
func (o omission) removeFrom() error { return os.RemoveAll(o.path) }

var omits = []omission{{"a"}}

type box struct{ v any }

func Copy() string {
	_ = slices.Clone(omits)
	return fmt.Sprint(box{})
}

func Kept() string {
	_ = strs.Keep(omits)
	return fmt.Sprint(box{})
}
