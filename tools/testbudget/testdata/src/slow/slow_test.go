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

// Runs 150ms, pauses at t.Parallel, resumes, and runs 100ms: its duration
// is the two runs, 250ms, counted once each.
func TestParallel150msBeforePauseThen100ms(t *testing.T) {
	time.Sleep(150 * time.Millisecond)
	t.Parallel()
	time.Sleep(100 * time.Millisecond)
}

// A serial parent of two parallel subtests: go test reports the parent at
// 0.00s, because the subtests run after its body returns. Its clock spans
// them, so it is listed at their 320ms.
func TestSerialParentOfTwoParallel320msSubtests(t *testing.T) {
	for _, name := range []string{"a", "b"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			time.Sleep(320 * time.Millisecond)
		})
	}
}
