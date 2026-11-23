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

// TestMain's store is no initializer's, so it earns the test nothing: now
// still holds what its initializer stored, the real clock.
func TestStampUnderTestMainsClockStillTouchesTheClock(t *testing.T) {
	_ = Stamp()
}
