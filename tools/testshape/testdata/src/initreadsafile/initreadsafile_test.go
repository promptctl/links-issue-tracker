// Package initreadsafile reads a file while the package initializes: that
// touch is the package's, not its test's.
package initreadsafile

import (
	"os"
	"testing"
)

var config, _ = os.ReadFile("x")

func TestReadingWhatInitReadTouchesNothing(t *testing.T) { _ = len(config) }
