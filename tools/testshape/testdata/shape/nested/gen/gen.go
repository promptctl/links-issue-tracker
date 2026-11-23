// Package gen is outside code generic over its type arguments: one generic
// per way a type argument's value can leave a generic's body.
package gen

import (
	"fmt"
	"io"
	"unsafe"
)

// Keep converts what it is handed to an interface.
func Keep[T any](v T) any { return v }

// Show prints what it is handed, which converts it to an interface.
func Show[T any](v T) string { return fmt.Sprintf("%v", v) }

// Twice converts nothing itself: it hands what it is handed to Keep.
func Twice[T any](v T) any { return Keep(v) }

// Visit registers what it is handed, as an interface, for Serve; the
// fixture classification calls it dispatching.
func Visit[T any](v T) { visitors = append(visitors, v) }

var visitors []any

// Serve calls back each registered visitor.
func Serve() {
	for _, v := range visitors {
		if s, ok := v.(interface{ Visit() }); ok {
			s.Visit()
		}
	}
}

// Namer returns the method expression T.String, a thunk that calls the
// constraint method on what it is handed.
func Namer[T fmt.Stringer]() func(T) string { return T.String }

// Address takes what it is handed as an unsafe.Pointer, which the reading
// does not model.
func Address[T any](v T) uintptr { return uintptr(unsafe.Pointer(&v)) }

// Box converts what it is handed inside a type its body declares, which
// the reading cannot bind outside the body.
func Box[T any](v T) any {
	type box struct{ v T }
	return box{v}
}

// Shut calls its constraint's Close on what it is handed.
func Shut[T io.Closer](c T) error { return c.Close() }

// First converts its first argument to an interface, and does nothing with
// its second.
func First[A, B any](a A, b B) any { return a }

// CloseFirst calls its constraint's Close on its first argument, and does
// nothing with its second.
func CloseFirst[P io.Closer, D any](p P, d D) error { return p.Close() }

// Shelf shows the zero value of its type argument: a Shelf holds no value of
// it.
type Shelf[T any] struct{}

// Show converts the zero value of T to an interface.
func (Shelf[T]) Show() string {
	var zero T
	return fmt.Sprint(zero)
}

// Outer embeds a Shelf, which promotes Show.
type Outer[T any] struct{ Shelf[T] }

// Slice converts a slice of what it is handed.
func Slice[T any](v T) any { return []T{v} }

// Map converts a map of what it is handed.
func Map[K comparable, V any](k K, v V) any { return map[K]V{k: v} }

// Field converts a struct with a tagged field holding what it is handed.
func Field[T any](v T) any {
	return struct {
		F T `json:"f"`
	}{v}
}

// Pointer converts a pointer to what it is handed.
func Pointer[T any](v T) any { return &v }

// Func converts a function taking a T: it holds no T.
func Func[T any](T) any { return func(T) {} }

// Tag is a type over T that holds no T.
type Tag[T any] struct{}

// Label converts a Tag[T]: it holds no T.
func Label[T any]() any { return Tag[T]{} }

// holder holds a T, and gets it.
type holder[T any] struct{ v T }

func (h holder[T]) Get() T { return h.v }

// Hold converts a holder of what it is handed to an interface with a
// method.
func Hold[T any](v T) any {
	var g interface{ Get() T } = holder[T]{v}
	return g
}

// Bag holds what is added to it, as interfaces.
type Bag[T any] struct{ items []any }

// Add converts what it is handed to an interface.
func (b *Bag[T]) Add(v T) { b.items = append(b.items, v) }
