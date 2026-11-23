// Package genericthunk calls a module type's method through a method
// expression an outside generic makes of its constraint method.
package genericthunk

import (
	"os"

	"example.com/shape/nested/gen"
)

type file string

func (f file) String() string {
	b, _ := os.ReadFile(string(f))
	return string(b)
}

func Name() string { return gen.Namer[file]()("a") }
