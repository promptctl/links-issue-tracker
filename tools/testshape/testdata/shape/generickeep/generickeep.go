// Package generickeep hands a module type to an outside generic that
// converts it to an interface, then prints the interface with %#v, which
// calls its GoString.
package generickeep

import (
	"fmt"
	"os"

	"example.com/shape/nested/gen"
)

type doc struct{ path string }

func (d doc) GoString() string {
	b, _ := os.ReadFile(d.path)
	return string(b)
}

func Render() string { return fmt.Sprintf("%#v", gen.Keep(doc{"a"})) }
