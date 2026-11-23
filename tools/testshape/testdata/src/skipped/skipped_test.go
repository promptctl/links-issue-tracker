// Package skipped has a test that skips, and would touch the filesystem if it
// ran.
package skipped

import (
	"os"
	"testing"
)

func TestSkipsBeforeItTouchesTheFilesystem(t *testing.T) {
	t.Skip("never runs")
	_, _ = os.ReadFile("x")
}
