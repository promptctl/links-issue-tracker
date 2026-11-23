// Package externalvarhandedon's init function hands io.Discard to a helper
// that reads a file: the helper writes this package's variable, not
// io.Discard.
package externalvarhandedon

import (
	"io"
	"os"
)

var out io.Writer

func setOut(w io.Writer) {
	out = w
	_, _ = os.ReadFile("x")
}

func init() { setOut(io.Discard) }
