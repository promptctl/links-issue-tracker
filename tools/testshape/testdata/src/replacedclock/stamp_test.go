package replacedclock

import (
	"testing"
	"time"
)

// Nothing replaces now: its initializer, time.Now, runs.
func TestStampOnTheRealClockTouchesTheClock(t *testing.T) {
	_ = Stamp()
}

// The test stores its own clock into now, so the initializer is dropped.
func TestStampOnAReplacedClockTouchesNothing(t *testing.T) {
	orig := now
	now = func() time.Time { return time.Time{} }
	defer func() { now = orig }()
	_ = Stamp()
}
