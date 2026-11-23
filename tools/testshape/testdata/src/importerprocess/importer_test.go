// Package importerprocess type-checks with the default importer, which runs
// `go list` inside the standard library to find export data.
package importerprocess

import (
	"go/importer"
	"testing"
)

func TestTheDefaultImporterIsAnotherProcess(t *testing.T) {
	if _, err := importer.Default().Import("fmt"); err != nil {
		t.Fatal(err)
	}
}
