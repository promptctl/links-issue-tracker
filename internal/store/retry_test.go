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

func TestRetryTransientManifestReadOnlyRetriesTransientError(t *testing.T) {
	op := &fakeRetryOperation{
		results: []error{
			transientManifestReadOnlyError{err: errors.New("transient manifest read only")},
			nil,
		},
	}

	err := retryTransientManifestReadOnly(
		context.Background(),
		op.run,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil {
		t.Fatalf("retryTransientManifestReadOnly() error = %v", err)
	}
	if op.calls != 2 {
		t.Fatalf("op.calls = %d, want 2", op.calls)
	}
}

func TestRetryTransientManifestReadOnlyReturnsLastErrorAfterExhaustion(t *testing.T) {
	results := make([]error, 0, transientManifestRetryMaxAttempts)
	for attempt := 1; attempt < transientManifestRetryMaxAttempts; attempt++ {
		results = append(results, transientManifestReadOnlyError{err: errors.New("transient")})
	}
	lastErr := transientManifestReadOnlyError{err: errors.New("transient final")}
	results = append(results, lastErr)
	op := &fakeRetryOperation{results: results}

	err := retryTransientManifestReadOnly(
		context.Background(),
		op.run,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if err == nil {
		t.Fatal("retryTransientManifestReadOnly() error = nil, want non-nil")
	}
	if !errors.Is(err, ErrTransientManifestReadOnly) {
		t.Fatalf("error = %v, want ErrTransientManifestReadOnly", err)
	}
	if err.Error() != lastErr.Error() {
		t.Fatalf("error = %q, want %q", err.Error(), lastErr.Error())
	}
	if op.calls != transientManifestRetryMaxAttempts {
		t.Fatalf("op.calls = %d, want %d", op.calls, transientManifestRetryMaxAttempts)
	}
}

func TestRetryTransientManifestReadOnlyDoesNotRetryNonTransientError(t *testing.T) {
	op := &fakeRetryOperation{
		results: []error{
			errors.New("some other storage failure"),
			nil,
		},
	}

	err := retryTransientManifestReadOnly(
		context.Background(),
		op.run,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if err == nil {
		t.Fatal("retryTransientManifestReadOnly() error = nil, want non-nil")
	}
	if op.calls != 1 {
		t.Fatalf("op.calls = %d, want 1", op.calls)
	}
}

func TestRetryTransientManifestReadOnlyHonorsContextTimeoutDuringBackoff(t *testing.T) {
	op := &fakeRetryOperation{
		results: []error{
			transientManifestReadOnlyError{err: errors.New("transient timeout")},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	err := retryTransientManifestReadOnly(
		ctx,
		op.run,
		func(int) time.Duration { return 50 * time.Millisecond },
		waitWithContext,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retryTransientManifestReadOnly() error = %v, want context.DeadlineExceeded", err)
	}
	if op.calls != 1 {
		t.Fatalf("op.calls = %d, want 1", op.calls)
	}
}

func TestTransientManifestRetryDelayIsBounded(t *testing.T) {
	for attempt := 1; attempt <= 10; attempt++ {
		delay := transientManifestRetryDelay(attempt)
		if delay < transientManifestRetryBaseDelay {
			t.Fatalf("delay(%d) = %v, want >= %v", attempt, delay, transientManifestRetryBaseDelay)
		}
		if delay > transientManifestRetryMaxDelay {
			t.Fatalf("delay(%d) = %v, want <= %v", attempt, delay, transientManifestRetryMaxDelay)
		}
	}
}

func TestWrapCommitWorkingSetErrorMarksTransientManifestReadOnly(t *testing.T) {
	err := wrapCommitWorkingSetError(errors.New("Error 1105: cannot update manifest: database is read only"))
	if !errors.Is(err, ErrTransientManifestReadOnly) {
		t.Fatalf("errors.Is(err, ErrTransientManifestReadOnly) = false, err=%v", err)
	}
	if !strings.Contains(err.Error(), "dolt commit working set") || !strings.Contains(err.Error(), "cannot update manifest") {
		t.Fatalf("unexpected wrapped error text: %q", err.Error())
	}
}

func TestWrapCommitWorkingSetErrorLeavesNonTransientUnmarked(t *testing.T) {
	err := wrapCommitWorkingSetError(errors.New("permission denied"))
	if errors.Is(err, ErrTransientManifestReadOnly) {
		t.Fatalf("errors.Is(err, ErrTransientManifestReadOnly) = true, err=%v", err)
	}
	if got, want := err.Error(), "dolt commit working set: permission denied"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestClassifyTransientManifestErrorWrapsGenericManifestFailures(t *testing.T) {
	err := classifyTransientManifestError(errors.New("commit add comment: Error 1105: cannot update manifest: database is read only"))
	if !errors.Is(err, ErrTransientManifestReadOnly) {
		t.Fatalf("errors.Is(err, ErrTransientManifestReadOnly) = false, err=%v", err)
	}
}

func TestClassifyTransientManifestErrorLeavesGenericFailures(t *testing.T) {
	source := errors.New("permission denied")
	err := classifyTransientManifestError(source)
	if err != source {
		t.Fatalf("classifyTransientManifestError() = %v, want original %v", err, source)
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
