// Package initreadsafile reads a file while the package initializes: the
// package's row carries it, and so does a test that uses what was read, as
// the code that computed it.
package initreadsafile

import (
	"os"
	"testing"
)

var config, _ = os.ReadFile("x")

func TestUsingWhatInitReadReachesTheRead(t *testing.T) { _ = len(config) }
