// Package genericpromote converts an outside generic type to a module
// interface whose method the type promotes from a generic type it embeds:
// the promotion wrapper runs the embedded type's method, an instance over
// the embedded type's type arguments.
package genericpromote

import (
	"os"

	"example.com/shape/nested/gen"
)

type doc string

func (d doc) Close() error { return os.Remove(string(d)) }

type shower interface{ Show() string }

func Show() string {
	var s shower = gen.Outer[doc]{}
	return s.Show()
}
