package slowpkg

import (
	"testing"
	"time"
)

func TestParallel600ms(t *testing.T) {
	t.Parallel()
	time.Sleep(600 * time.Millisecond)
}

func TestSerial520ms(t *testing.T) {
	time.Sleep(520 * time.Millisecond)
}

func TestParallel440ms(t *testing.T) {
	t.Parallel()
	time.Sleep(440 * time.Millisecond)
}

func TestSerial360ms(t *testing.T) {
	time.Sleep(360 * time.Millisecond)
}

func TestParallel280ms(t *testing.T) {
	t.Parallel()
	time.Sleep(280 * time.Millisecond)
}

func TestSerial200ms(t *testing.T) {
	time.Sleep(200 * time.Millisecond)
}

func TestParallel120ms(t *testing.T) {
	t.Parallel()
	time.Sleep(120 * time.Millisecond)
}

func TestSerial80ms(t *testing.T) {
	time.Sleep(80 * time.Millisecond)
}

func TestSerial40ms(t *testing.T) {
	time.Sleep(40 * time.Millisecond)
}

func TestSerial10ms(t *testing.T) {
	time.Sleep(10 * time.Millisecond)
}

// A serial parent whose subtest outranks most top-level tests: only the
// parent may be listed.
func TestSerialParentOfA400msSubtest(t *testing.T) {
	t.Run("slow", func(t *testing.T) { time.Sleep(400 * time.Millisecond) })
}
