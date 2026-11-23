package externalvarhandedon

import (
	"io"
	"testing"
)

// io.Discard is the standard library's variable, and an interface value
// handed to a call cannot be written through.
func TestUsingAVariableAnInitHandsOnTouchesNothing(t *testing.T) { _, _ = io.Discard.Write(nil) }
