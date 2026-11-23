// Package gen is outside code generic over its type arguments: one generic
// per way a type argument's value can leave a generic's body.
package gen

import (
	"fmt"
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
