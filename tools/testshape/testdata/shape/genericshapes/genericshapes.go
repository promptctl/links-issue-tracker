// Package genericshapes hands module types to outside generics that
// convert a type made of their type argument: a slice, a map, a struct with
// a tagged field, a pointer, an outside type holding it behind an
// interface with a method; and two that convert a type over it that holds
// none of it, a func taking it and a type over it with no field.
package genericshapes

import (
	"net/http"
	"os"

	"example.com/shape/nested/gen"
)

type doc string

func (d doc) Close() error { return os.Remove(string(d)) }

type page string

func (p page) Fetch() { http.Get(string(p)) }

func InSlice() any { return gen.Slice(doc("a")) }

func InMap() any { return gen.Map(doc("a"), page("b")) }

func InField() any { return gen.Field(doc("a")) }

func InPointer() any { return gen.Pointer(doc("a")) }

func InHolder() any { return gen.Hold(doc("a")) }

func InFunc() any { return gen.Func(doc("a")) }

func InTag() any { return gen.Label[doc]() }
