package storage

import (
	"testing"
	"time"
)

// Every example the reference prints is a selector the grammar reads, so the
// help never teaches one a listing refuses.
func TestEverySelectorExampleParses(t *testing.T) {
	for _, example := range SelectorExamples {
		if _, err := ParseSelector(example.Text, time.Now()); err != nil {
			t.Errorf("example %q does not parse: %v", example.Text, err)
		}
	}
}
