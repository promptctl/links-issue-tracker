// Package genericbound takes a method of an outside generic type as a
// bound method value, whose wrapper carries no type arguments of its own:
// its receiver type's are the instance's.
package genericbound

import (
	"os"

	"example.com/shape/nested/gen"
)

type doc string

func (d doc) Close() error { return os.Remove(string(d)) }

func Fill() {
	var b gen.Bag[doc]
	add := b.Add
	add(doc("a"))
}
