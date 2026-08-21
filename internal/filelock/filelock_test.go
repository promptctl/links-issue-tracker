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

// TestAcquire_EntryGateRefusesDoneContext pins the entry gate: a context that
// is already done is refused before any attempt — free lock or held — because
// an interrupted command must not acquire (and then hold) a lock on its way
// down, admitting its guarded operation into the shutdown grace; and against
// a held lock the refusal must read as cancellation, never as "healthy
// holder, retry" contention.
func TestAcquire_EntryGateRefusesDoneContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Free lock: refused, and left free for the next live caller.
	freePath := filepath.Join(t.TempDir(), "free.lock")
	release, acquired, err := Acquire(ctx, freePath, true, 1, 0)
	if acquired {
		_ = release()
		t.Fatal("acquired a free lock under an already-canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("free lock: want context.Canceled, got err=%v", err)
	}
	release, acquired, err = Acquire(context.Background(), freePath, true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("free lock after refused acquire: acquired=%v err=%v", acquired, err)
	}
	if err := release(); err != nil {
		t.Errorf("release: %v", err)
	}

	// Held lock: still cancellation, never clean contention.
	heldPath := filepath.Join(t.TempDir(), "held.lock")
	holder, acquired, err := Acquire(context.Background(), heldPath, true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("holder: acquired=%v err=%v", acquired, err)
	}
	defer func() {
		if err := holder(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	}()
	_, acquired, err = Acquire(ctx, heldPath, true, 1, 0)
	if acquired {
		t.Fatal("acquired under a held exclusive lock")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("held lock: want context.Canceled, got err=%v", err)
	}
}

// TestAcquire_CancelDuringRetrySurfacesCancellation pins the mid-wait path:
// a context live at entry (so the entry gate passes) that becomes done while
// Acquire is retrying against a live holder surfaces as cancellation from
// the retry sleep — an interrupted process stops waiting immediately, and is
// never told clean contention.
func TestAcquire_CancelDuringRetrySurfacesCancellation(t *testing.T) {
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

	// The budget (10s of 20ms attempts) far outlasts the 50ms cancel, so the
	// cancellation deterministically lands inside a retry sleep, not at entry
	// and not at exhaustion.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, acquired, err = Acquire(ctx, lockPath, true, 500, 20*time.Millisecond)
	if acquired {
		t.Fatal("acquired under a held exclusive lock")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled from the retry sleep, got err=%v", err)
	}
}

// TestAcquire_RejectsNonPositiveBudget pins that an impossible retry budget
// is a loud caller bug, not a silent "clean contention" verdict on a lock
// nobody holds.
func TestAcquire_RejectsNonPositiveBudget(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	for _, attempts := range []int{0, -1} {
		_, acquired, err := Acquire(context.Background(), lockPath, true, attempts, 0)
		if acquired || err == nil {
			t.Fatalf("maxAttempts=%d: want loud error, got acquired=%v err=%v", attempts, acquired, err)
		}
	}
}
