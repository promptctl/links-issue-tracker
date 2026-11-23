// Package genericpair hands an outside generic two module types, the first
// of which it converts, and the second of which it does nothing with.
package genericpair

import (
	"net/http"
	"os"

	"example.com/shape/nested/gen"
)

type doc string

func (d doc) Close() error { return os.Remove(string(d)) }

type page string

func (p page) Fetch() { http.Get(string(p)) }

func Pick() any { return gen.First(doc("a"), page("b")) }

// PickSame hands the generic one type twice: it converts the first, and
// does nothing with the second.
func PickSame() any { return gen.First(doc("a"), doc("b")) }
