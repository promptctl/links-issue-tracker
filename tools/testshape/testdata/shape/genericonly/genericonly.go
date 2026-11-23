// Package genericonly hands an outside generic a pointer to a module type
// and the type itself: the generic calls only Close on the pointer, through
// its constraint, and does nothing with the type.
package genericonly

import (
	"net/http"
	"os"

	"example.com/shape/nested/gen"
)

type doc string

func (d doc) Close() error { return os.Remove(string(d)) }

func (d doc) Fetch() { http.Get(string(d)) }

func Shut() error {
	d := doc("a")
	return gen.CloseFirst(&d, d)
}
