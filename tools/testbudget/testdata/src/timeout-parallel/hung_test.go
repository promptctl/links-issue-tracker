package hungparpkg

import (
	"testing"
	"time"
)

// Runs 200ms, pauses at t.Parallel, resumes, and hangs: its duration is
// run-to-pause plus cont-to-timeout.
func TestParallel200msBeforePauseThenHangs(t *testing.T) {
	time.Sleep(200 * time.Millisecond)
	t.Parallel()
	select {}
}

func TestSerial100ms(t *testing.T) { time.Sleep(100 * time.Millisecond) }
