// Package genericlocals declares two types named file, alike in all but the
// function body that declares each, and hands each to an outside generic
// whose body the reading cannot decide: two types, so two instances.
package genericlocals

import "example.com/shape/nested/gen"

func Read() uintptr {
	type file string
	return gen.Address(file("a"))
}

func Write() uintptr {
	type file string
	return gen.Address(file("b"))
}
