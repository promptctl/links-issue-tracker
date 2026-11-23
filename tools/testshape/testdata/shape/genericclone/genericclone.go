// Package genericclone hands a module type with a touching method to
// slices.Clone, whose constraint names no method it could call.
package genericclone

import (
	"os"
	"slices"
)

type omission struct{ path string }

// removeFrom is never called: not by this package, and not by slices.Clone.
func (o omission) removeFrom() error { return os.RemoveAll(o.path) }

var omits = []omission{{"a"}}

func Copy() []omission { return slices.Clone(omits) }
