// Package genericvisit registers a module type with a dispatching outside
// generic, which calls back what it registered.
package genericvisit

import (
	"os"

	"example.com/shape/nested/gen"
)

type cleaner struct{ dir string }

func (c cleaner) Visit() { os.RemoveAll(c.dir) }

func Register() { gen.Visit(cleaner{"tmp"}) }
