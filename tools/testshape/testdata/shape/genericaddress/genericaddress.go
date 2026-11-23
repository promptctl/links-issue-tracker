// Package genericaddress hands a module type to an outside generic whose
// body the reading cannot decide: it takes its argument as an
// unsafe.Pointer.
package genericaddress

import (
	"os"

	"example.com/shape/nested/gen"
)

type lock string

func (l lock) Release() { os.Remove(string(l)) }

func Where() uintptr { return gen.Address(lock("a")) }
