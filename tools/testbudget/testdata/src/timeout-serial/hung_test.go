package hungpkg

import (
	"testing"
	"time"
)

func TestSerial100ms(t *testing.T) { time.Sleep(100 * time.Millisecond) }

// Still paused at t.Parallel when the timeout fires: parallel tests resume
// only after every serial top-level test returns, and TestSerialHangs never
// does. Its 300ms before the pause is its whole duration.
func TestParallel300msBeforePauseNeverResumed(t *testing.T) {
	time.Sleep(300 * time.Millisecond)
	t.Parallel()
	time.Sleep(50 * time.Millisecond)
}

// Hangs inside a subtest: the parent and the subtest are both running when
// the timeout fires; only the parent may be listed.
func TestSerialHangs(t *testing.T) {
	t.Run("blocks", func(t *testing.T) { select {} })
}
