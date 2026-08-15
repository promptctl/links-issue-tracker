package filelock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestAcquire_SharedHoldersCoexist pins the reader side of the contract:
// shared holds on independent FDs never contend with each other.
func TestAcquire_SharedHoldersCoexist(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	first, acquired, err := Acquire(context.Background(), lockPath, false, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("first shared: acquired=%v err=%v", acquired, err)
	}
	defer func() {
		if err := first(); err != nil {
			t.Errorf("release first: %v", err)
		}
	}()
	second, acquired, err := Acquire(context.Background(), lockPath, false, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("second shared must coexist: acquired=%v err=%v", acquired, err)
	}
	if err := second(); err != nil {
		t.Errorf("release second: %v", err)
	}
}

// TestAcquire_ExclusiveProbeReportsContentionAsValue pins the three-outcome
// shape: while a shared holder is live, a single-attempt exclusive probe
// reports (acquired=false, err=nil) — contention is a legal outcome of a
// healthy lock, not a failure — and after the holder releases, the same
// probe acquires. This value-not-error contract is what lets a caller use
// exclusive acquisition as a kernel liveness proof over shared holders.
func TestAcquire_ExclusiveProbeReportsContentionAsValue(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	holder, acquired, err := Acquire(context.Background(), lockPath, false, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("shared holder: acquired=%v err=%v", acquired, err)
	}

	release, acquired, err := Acquire(context.Background(), lockPath, true, 1, 0)
	if err != nil {
		t.Fatalf("contention must be a value, not an error: %v", err)
	}
	if acquired {
		_ = release()
		t.Fatal("exclusive probe acquired while a shared holder was live")
	}

	if err := holder(); err != nil {
		t.Fatalf("release shared holder: %v", err)
	}
	release, acquired, err = Acquire(context.Background(), lockPath, true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("exclusive after release: acquired=%v err=%v", acquired, err)
	}
	if err := release(); err != nil {
		t.Errorf("release exclusive: %v", err)
	}
}

// TestAcquire_CanceledContextSurfacesDuringRetry pins that a canceled context
// aborts the retry sleep as an error (distinct from clean contention), so an
// interrupted process stops waiting on a lock immediately.
func TestAcquire_CanceledContextSurfacesDuringRetry(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	holder, acquired, err := Acquire(context.Background(), lockPath, true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("holder: acquired=%v err=%v", acquired, err)
	}
	defer func() {
		if err := holder(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, acquired, err = Acquire(ctx, lockPath, true, 5, 10*time.Millisecond)
	if acquired {
		t.Fatal("acquired under a held exclusive lock")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled from the retry sleep, got %v", err)
	}
}
