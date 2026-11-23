// Package stringer has a type whose String method reads a file, nested in a
// struct it prints.
package stringer

import (
	"fmt"
	"os"
)

type path string

func (p path) String() string {
	b, _ := os.ReadFile(string(p))
	return string(b)
}

type report struct{ at path }

func Describe() string { return fmt.Sprint(report{at: "config"}) }
