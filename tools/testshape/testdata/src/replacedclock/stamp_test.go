package replacedclock

import (
	"testing"
	"time"
)

// now holds its initializer's time.Now.
func TestStampOnTheRealClockTouchesTheClock(t *testing.T) {
	_ = Stamp()
}

// The test stores its own clock into now, and earns nothing for it: now
// still holds what its initializer stored, the real clock.
func TestStampOnAReplacedClockStillTouchesTheClock(t *testing.T) {
	orig := now
	now = func() time.Time { return time.Time{} }
	defer func() { now = orig }()
	_ = Stamp()
}
