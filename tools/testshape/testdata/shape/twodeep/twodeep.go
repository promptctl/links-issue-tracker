// Package twodeep reaches the filesystem two calls below its test.
package twodeep

import "os"

func Load(name string) []byte { return read(name) }

func read(name string) []byte {
	b, _ := os.ReadFile(name)
	return b
}
