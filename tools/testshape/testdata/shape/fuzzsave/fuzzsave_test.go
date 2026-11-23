// Package fuzzsave hands its fuzz function to f.Fuzz as an any, which calls
// it through reflection.
package fuzzsave

import (
	"os"
	"testing"
)

func FuzzSave(f *testing.F) {
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = os.WriteFile("saved", b, 0o600)
	})
}
