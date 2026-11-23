// Package funcinstruct runs the funcs held in a slice of structs, one of
// which reads a file.
package funcinstruct

import "os"

type fix struct {
	name string
	run  func() error
}

var fixes = []fix{
	{"read", func() error { _, err := os.ReadFile("x"); return err }},
}

// RunFixes runs every fix.
func RunFixes() {
	for _, f := range fixes {
		_ = f.run()
	}
}
