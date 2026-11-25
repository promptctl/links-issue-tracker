package stamp_test

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model/stamp"
)

// An Instant Next did not build is the zero literal any package can write.
// Reading it fails loudly, so a forgotten stamp is never recorded as the
// year 1.
func TestTimeRefusesAnInstantNextDidNotBuild(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Time() on the zero Instant returned; want a panic")
		}
	}()
	var unbuilt stamp.Instant
	_ = unbuilt.Time()
}

// Ahead is how far the stamp stepped past the clock: zero when the clock was
// past the history, and the step past history otherwise.
func TestAheadIsTheStepPastTheClock(t *testing.T) {
	clock := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if got := stamp.Next(clock, clock.Add(-time.Minute)).Ahead(clock); got != 0 {
		t.Errorf("Ahead of a clock past history = %v, want 0", got)
	}
	if got := stamp.Next(clock, clock.Add(time.Minute)).Ahead(clock); got != time.Minute+time.Nanosecond {
		t.Errorf("Ahead of a clock behind history = %v, want %v", got, time.Minute+time.Nanosecond)
	}
}
