package testmainclock

import (
	"os"
	"testing"
	"time"
)

// TestMain stores a fixed clock into now for every test.
func TestMain(m *testing.M) {
	now = func() time.Time { return time.Time{} }
	os.Exit(m.Run())
}

func TestStampUnderTestMainsClockTouchesNothing(t *testing.T) {
	_ = Stamp()
}
