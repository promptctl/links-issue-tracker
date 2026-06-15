package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRetryOperation struct {
	results []error
	calls   int
}

func (f *fakeRetryOperation) run(_ context.Context) error {
	f.calls++
	if len(f.results) == 0 {
		return errors.New("unexpected call")
	}
	current := f.results[0]
	f.results = f.results[1:]
	return current
}

// noRotate is the rotate hook for retry tests that don't exercise reconnection.
func noRotate() error { return nil }

func TestRetryTransientGCContentionRetriesTransientError(t *testing.T) {
	op := &fakeRetryOperation{
		results: []error{
			transientGCContentionError{err: errors.New("transient manifest read only")},
			nil,
		},
	}

	err := retryTransientGCContention(
		context.Background(),
		op.run,
		noRotate,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil {
		t.Fatalf("retryTransientGCContention() error = %v", err)
	}
	if op.calls != 2 {
		t.Fatalf("op.calls = %d, want 2", op.calls)
	}
}

func TestRetryTransientGCContentionReturnsLastErrorAfterExhaustion(t *testing.T) {
	results := make([]error, 0, transientRetryMaxAttempts)
	for attempt := 1; attempt < transientRetryMaxAttempts; attempt++ {
		results = append(results, transientGCContentionError{err: errors.New("transient")})
	}
	lastErr := transientGCContentionError{err: errors.New("transient final")}
	results = append(results, lastErr)
	op := &fakeRetryOperation{results: results}

	err := retryTransientGCContention(
		context.Background(),
		op.run,
		noRotate,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if err == nil {
		t.Fatal("retryTransientGCContention() error = nil, want non-nil")
	}
	if !errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("error = %v, want ErrTransientGCContention", err)
	}
	if err.Error() != lastErr.Error() {
		t.Fatalf("error = %q, want %q", err.Error(), lastErr.Error())
	}
	if op.calls != transientRetryMaxAttempts {
		t.Fatalf("op.calls = %d, want %d", op.calls, transientRetryMaxAttempts)
	}
}

func TestRetryTransientGCContentionDoesNotRetryNonTransientError(t *testing.T) {
	op := &fakeRetryOperation{
		results: []error{
			errors.New("some other storage failure"),
			nil,
		},
	}

	err := retryTransientGCContention(
		context.Background(),
		op.run,
		noRotate,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if err == nil {
		t.Fatal("retryTransientGCContention() error = nil, want non-nil")
	}
	if op.calls != 1 {
		t.Fatalf("op.calls = %d, want 1", op.calls)
	}
}

func TestRetryTransientGCContentionHonorsContextTimeoutDuringBackoff(t *testing.T) {
	op := &fakeRetryOperation{
		results: []error{
			transientGCContentionError{err: errors.New("transient timeout")},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	err := retryTransientGCContention(
		ctx,
		op.run,
		noRotate,
		func(int) time.Duration { return 50 * time.Millisecond },
		waitWithContext,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retryTransientGCContention() error = %v, want context.DeadlineExceeded", err)
	}
	if op.calls != 1 {
		t.Fatalf("op.calls = %d, want 1", op.calls)
	}
}

// TestRetryTransientGCContentionRotatesConnectionBetweenAttempts pins the fix:
// the GC reset poisons the connection, so the retry must rotate it before each
// re-attempt. One rotation per backoff, never after the final (succeeding) call.
func TestRetryTransientGCContentionRotatesConnectionBetweenAttempts(t *testing.T) {
	op := &fakeRetryOperation{
		results: []error{
			transientGCContentionError{err: errors.New("gc reset 1")},
			transientGCContentionError{err: errors.New("gc reset 2")},
			nil,
		},
	}
	rotations := 0

	err := retryTransientGCContention(
		context.Background(),
		op.run,
		func() error { rotations++; return nil },
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil {
		t.Fatalf("retryTransientGCContention() error = %v", err)
	}
	if op.calls != 3 {
		t.Fatalf("op.calls = %d, want 3", op.calls)
	}
	if rotations != 2 {
		t.Fatalf("rotations = %d, want 2 (one per backoff, none after success)", rotations)
	}
}

// TestRetryTransientGCContentionSurfacesRotateFailure proves a failed reconnect
// aborts the retry loudly instead of silently looping on a dead connection.
// [LAW:no-silent-failure]
func TestRetryTransientGCContentionSurfacesRotateFailure(t *testing.T) {
	op := &fakeRetryOperation{
		results: []error{
			transientGCContentionError{err: errors.New("gc reset")},
			nil,
		},
	}
	rotateErr := errors.New("reopen dolt failed")

	err := retryTransientGCContention(
		context.Background(),
		op.run,
		func() error { return rotateErr },
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if !errors.Is(err, rotateErr) {
		t.Fatalf("retryTransientGCContention() error = %v, want rotate failure", err)
	}
	if op.calls != 1 {
		t.Fatalf("op.calls = %d, want 1 (no re-attempt after rotate failure)", op.calls)
	}
}

func TestTransientRetryDelayIsBounded(t *testing.T) {
	for attempt := 1; attempt <= 10; attempt++ {
		delay := transientRetryDelay(attempt)
		if delay < transientRetryBaseDelay {
			t.Fatalf("delay(%d) = %v, want >= %v", attempt, delay, transientRetryBaseDelay)
		}
		if delay > transientRetryMaxDelay {
			t.Fatalf("delay(%d) = %v, want <= %v", attempt, delay, transientRetryMaxDelay)
		}
	}
}

func TestWrapCommitWorkingSetErrorMarksManifestReadOnly(t *testing.T) {
	err := wrapCommitWorkingSetError(errors.New("Error 1105: cannot update manifest: database is read only"))
	if !errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("errors.Is(err, ErrTransientGCContention) = false, err=%v", err)
	}
	if !strings.Contains(err.Error(), "dolt commit working set") || !strings.Contains(err.Error(), "cannot update manifest") {
		t.Fatalf("unexpected wrapped error text: %q", err.Error())
	}
}

// TestWrapCommitWorkingSetErrorMarksGCReset covers the previously-unhandled
// variant: a commit that hits Dolt's online-GC connection invalidation must be
// classified transient so it is retried (with a reconnect), not surfaced raw.
func TestWrapCommitWorkingSetErrorMarksGCReset(t *testing.T) {
	err := wrapCommitWorkingSetError(errors.New("this connection was established when this server performed an online garbage collection. this connection can no longer be used. please reconnect."))
	if !errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("errors.Is(err, ErrTransientGCContention) = false, err=%v", err)
	}
}

func TestWrapCommitWorkingSetErrorLeavesNonTransientUnmarked(t *testing.T) {
	err := wrapCommitWorkingSetError(errors.New("permission denied"))
	if errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("errors.Is(err, ErrTransientGCContention) = true, err=%v", err)
	}
	if got, want := err.Error(), "dolt commit working set: permission denied"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestClassifyTransientGCErrorWrapsManifestReadOnly(t *testing.T) {
	err := classifyTransientGCError(errors.New("commit add comment: Error 1105: cannot update manifest: database is read only"))
	if !errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("errors.Is(err, ErrTransientGCContention) = false, err=%v", err)
	}
}

func TestClassifyTransientGCErrorWrapsGCReset(t *testing.T) {
	err := classifyTransientGCError(errors.New("gc_copier: this connection was established when this server performed an online garbage collection. please reconnect."))
	if !errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("errors.Is(err, ErrTransientGCContention) = false, err=%v", err)
	}
}

// TestClassifyTransientGCErrorLeavesClusterRoleReconnect guards the precision of
// the GC-reset predicate: the cluster role-transition error also says "please
// reconnect" but is not GC contention, so it must NOT be retried as transient.
func TestClassifyTransientGCErrorLeavesClusterRoleReconnect(t *testing.T) {
	source := errors.New("this server transitioned cluster roles. this connection can no longer be used. please reconnect.")
	err := classifyTransientGCError(source)
	if errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("cluster-role reconnect misclassified as GC contention: %v", err)
	}
}

func TestClassifyTransientGCErrorLeavesGenericFailures(t *testing.T) {
	source := errors.New("permission denied")
	err := classifyTransientGCError(source)
	if err != source {
		t.Fatalf("classifyTransientGCError() = %v, want original %v", err, source)
	}
}

func TestWithCommitLockSerializesConcurrentOperations(t *testing.T) {
	s := &Store{commitLockPath: filepath.Join(t.TempDir(), ".links-commit.lock")}
	firstEntered := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- s.withCommitLock(context.Background(), func(context.Context) error {
			firstEntered <- struct{}{}
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- s.withCommitLock(context.Background(), func(context.Context) error {
			close(secondEntered)
			return nil
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("second operation entered critical section before first released lock")
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseFirst)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("withCommitLock() error = %v", err)
		}
	}

	select {
	case <-secondEntered:
	default:
		t.Fatal("second operation never entered critical section")
	}
}
