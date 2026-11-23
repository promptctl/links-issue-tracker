package anothertestsfake

import (
	"os"
	"testing"
	"time"
)

// The fake is built in this test's reach, so this test carries its touch.
func TestStoringAFakeThatReadsAFileTouchesTheFilesystem(t *testing.T) {
	saved := now
	now = func() time.Time { _, _ = os.ReadFile("x"); return time.Time{} }
	defer func() { now = saved }()
}

// Another test's fake is not in this test's reach.
func TestAnotherTestsFakeIsNotCharged(t *testing.T) { _ = Stamp() }
