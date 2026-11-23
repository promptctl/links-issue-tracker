package pointerstoreintovar

import (
	"testing"
	"time"
)

func swap(p *func() time.Time, f func() time.Time) func() {
	old := *p
	*p = f
	return func() { *p = old }
}

// The fake stored through the pointer earns the test nothing: clock still
// holds what its initializer stored, the real clock.
func TestAClockSwappedThroughAPointerStillReadsTheClock(t *testing.T) {
	defer swap(&clock, func() time.Time { return time.Time{} })()
	_ = Stamp()
}
