package cli

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeDoltRetryResult struct {
	output string
	err    error
}

type fakeDoltRetryOperation struct {
	results []fakeDoltRetryResult
	calls   int
}

func (f *fakeDoltRetryOperation) run() (string, error) {
	f.calls++
	if len(f.results) == 0 {
		return "", errors.New("unexpected call")
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result.output, result.err
}

func TestRetryDoltManifestReadOnlyRetriesAndReturnsSuccessOutput(t *testing.T) {
	op := &fakeDoltRetryOperation{
		results: []fakeDoltRetryResult{
			{err: errors.New("cannot update manifest: database is read only")},
			{output: "ok"},
		},
	}

	output, err := retryDoltManifestReadOnly(
		context.Background(),
		op.run,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil {
		t.Fatalf("retryDoltManifestReadOnly() error = %v", err)
	}
	if output != "ok" {
		t.Fatalf("output = %q, want ok", output)
	}
	if op.calls != 2 {
		t.Fatalf("op.calls = %d, want 2", op.calls)
	}
}

func TestRetryDoltManifestReadOnlyReturnsNonManifestErrorWithoutRetry(t *testing.T) {
	op := &fakeDoltRetryOperation{
		results: []fakeDoltRetryResult{
			{err: errors.New("permission denied")},
			{output: "unexpected success"},
		},
	}

	_, err := retryDoltManifestReadOnly(
		context.Background(),
		op.run,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if err == nil {
		t.Fatal("retryDoltManifestReadOnly() error = nil, want non-nil")
	}
	if op.calls != 1 {
		t.Fatalf("op.calls = %d, want 1", op.calls)
	}
}

func TestRetryDoltManifestReadOnlyReturnsContextErrorDuringBackoff(t *testing.T) {
	op := &fakeDoltRetryOperation{
		results: []fakeDoltRetryResult{
			{err: errors.New("cannot update manifest: database is read only")},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	_, err := retryDoltManifestReadOnly(
		ctx,
		op.run,
		func(int) time.Duration { return 50 * time.Millisecond },
		waitWithContext,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retryDoltManifestReadOnly() error = %v, want context.DeadlineExceeded", err)
	}
	if op.calls != 1 {
		t.Fatalf("op.calls = %d, want 1", op.calls)
	}
}

func TestSyncManifestRetryDelayIsBounded(t *testing.T) {
	for attempt := 1; attempt <= 10; attempt++ {
		delay := syncManifestRetryDelay(attempt)
		if delay < syncManifestRetryBaseDelay {
			t.Fatalf("delay(%d) = %v, want >= %v", attempt, delay, syncManifestRetryBaseDelay)
		}
		if delay > syncManifestRetryMaxDelay {
			t.Fatalf("delay(%d) = %v, want <= %v", attempt, delay, syncManifestRetryMaxDelay)
		}
	}
}
