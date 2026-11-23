// Package genericclone hands a module type with a touching method to
// slices.Clone, whose body neither converts it nor calls a method of it.
package genericclone

import (
	"os"
	"slices"
)

type paths []string

func (p paths) Remove() {
	for _, path := range p {
		os.Remove(path)
	}
}

func Copy() paths { return slices.Clone(paths{"a"}) }
