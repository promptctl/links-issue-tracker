//go:build shapetag

package tagged

import (
	"os"
	"testing"
)

func TestTagged(t *testing.T) { _, _ = os.ReadFile("config") }
