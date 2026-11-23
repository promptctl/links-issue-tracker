// Package strs is outside code generic over its type arguments: an instance
// calls its constraint's methods on the values it is handed, and may convert
// them to an interface and pass them on.
package strs

import (
	"encoding/json"
	"fmt"
	"os"
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

// Visit hands x to what was registered to visit it.
func Visit[T any](x T) { visitor(x) }

var visitor = func(any) {}

// Keep returns x in an interface value.
func Keep[T any](x T) any { return x }

// Encode encodes x as JSON, which calls its MarshalJSON if it has one.
func Encode[T any](x T) ([]byte, error) { return json.Marshal(x) }

// Path is an outside type whose String touches the filesystem.
type Path string

func (p Path) String() string {
	b, _ := os.ReadFile(string(p))
	return string(b)
}
