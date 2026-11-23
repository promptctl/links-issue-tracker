// Package timed is a throwaway package whose go test -json stream
// record.sh records for tools/testshape's timing join. Each test's name
// states its designed time and touches.
package timed

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSleeps200ms(t *testing.T) {
	time.Sleep(200 * time.Millisecond)
}

func TestParentOfTwoParallelSubtestsThatSleep150ms(t *testing.T) {
	for _, name := range []string{"a", "b"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			time.Sleep(150 * time.Millisecond)
		})
	}
}

func TestParentOfANestedSubtestThatSleeps100ms(t *testing.T) {
	t.Run("outer", func(t *testing.T) {
		t.Run("inner", func(t *testing.T) {
			time.Sleep(100 * time.Millisecond)
		})
	})
}

func TestOnlyComputes(t *testing.T) {
	if 1+2 != 3 {
		t.Fatal("sum")
	}
}

func TestWritesATempFile(t *testing.T) {
	if err := os.WriteFile(filepath.Join(t.TempDir(), "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkips(t *testing.T) {
	t.Skip("skipped by design")
}
