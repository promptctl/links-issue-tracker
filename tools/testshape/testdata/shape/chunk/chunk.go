// Package chunk writes in a range-over-func loop: the loop's body is a
// function the iterator, outside code, is handed and calls back.
package chunk

import (
	"os"
	"slices"
)

func Save(names []string) {
	for c := range slices.Chunk(names, 2) {
		_ = os.WriteFile(c[0], nil, 0o600)
	}
}
