// Package genericlocal hands a module type to an outside generic whose body
// the reading cannot decide: it converts its argument inside a type the body
// declares.
package genericlocal

import (
	"os"

	"example.com/shape/nested/gen"
)

type lock string

func (l lock) Release() { os.Remove(string(l)) }

func Wrap() any { return gen.Box(lock("a")) }
