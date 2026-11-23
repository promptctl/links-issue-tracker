// Package strs is outside code generic over a constraint: an instance calls
// the String of whatever type argument it is given.
package strs

import (
	"fmt"
	"strings"
)

// Join joins each x's String.
func Join[T fmt.Stringer](xs []T) string {
	var b strings.Builder
	for _, x := range xs {
		b.WriteString(x.String())
	}
	return b.String()
}

// Show formats x, which calls its String if it has one.
func Show[T any](x T) string { return fmt.Sprint(x) }
