// Package genericjoin hands module types to outside generics as their type
// arguments; each generic reaches a touching method of the type its own way.
package genericjoin

import (
	"fmt"
	"os"
	"reflect"

	"example.com/shape/nested/strs"
)

type file string

func (f file) String() string {
	b, _ := os.ReadFile(string(f))
	return string(b)
}

// Names: the generic calls String through its constraint.
func Names() string { return strs.Join([]file{"a"}) }

// Shown: a generic whose constraint names no method calls String back by
// converting its argument to an interface.
func Shown() string { return strs.Show(file("b")) }

// ShownSlice: the file is an element of the type argument.
func ShownSlice() string { return strs.Show([]file{"c"}) }

type wrap struct{ F file }

// ShownField: the file is a field of the type argument.
func ShownField() string { return strs.Show(wrap{F: "d"}) }

// Visited: a dispatching generic reaches every method of what it is handed.
func Visited() { strs.Visit(file("e")) }

// KeptCalled: the generic hands the file back in an interface value, and
// reflection calls its method.
func KeptCalled() { reflect.ValueOf(strs.Keep(file("f"))).Method(0).Call(nil) }

// Stringers: the type argument is an interface, so the generic's String
// calls are interface calls, which reach the outside Path's String.
func Stringers() string { return strs.Join([]fmt.Stringer{strs.Path("g")}) }

type record struct{ path string }

func (r *record) MarshalJSON() ([]byte, error) { return os.ReadFile(r.path) }

// Encoded: a slice's element is addressable, so encoding calls the
// element's pointer-receiver MarshalJSON.
func Encoded() ([]byte, error) { return strs.Encode([]record{{"h"}}) }
