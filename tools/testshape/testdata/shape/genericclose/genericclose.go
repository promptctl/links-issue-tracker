// Package genericclose hands an outside generic a module interface as its
// type argument, holding an *os.File that reaches the interface only through
// any: only the generic's call of Close through its constraint, an
// interface call, reaches the file's Close.
package genericclose

import (
	"os"

	"example.com/shape/nested/gen"
)

type Closer interface{ Close() error }

func Release() error {
	var v any = os.Stdout
	return gen.Shut(v.(Closer))
}
