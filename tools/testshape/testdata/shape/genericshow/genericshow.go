// Package genericshow hands an outside generic that converts its argument a
// slice of a module type, and a struct holding one: the module type is in
// the structure of the type argument, not the type argument itself.
package genericshow

import (
	"os"

	"example.com/shape/nested/gen"
)

type file string

func (f file) String() string {
	b, _ := os.ReadFile(string(f))
	return string(b)
}

type wrap struct{ F file }

func ShowSlice() string { return gen.Show([]file{"a"}) }

func ShowStruct() string { return gen.Show(wrap{F: "a"}) }
