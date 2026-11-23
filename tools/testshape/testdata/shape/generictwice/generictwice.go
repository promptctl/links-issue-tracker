// Package generictwice hands a module type to an outside generic that
// converts nothing itself, but instantiates another generic over it, which
// converts it to an interface.
package generictwice

import (
	"os"

	"example.com/shape/nested/gen"
)

type doc string

func (d doc) Close() error { return os.Remove(string(d)) }

func Hold() any { return gen.Twice(doc("a")) }
