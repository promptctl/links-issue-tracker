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
func noRotate(context.Context) error { return nil }

func TestRetryTransientGCContentionRetriesTransientError(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestRetryTransientGCContentionPromotesExhaustedManifestReadOnly pins defect #3
// of links-sync-s3r6: a manifest-read-only that survives the entire retry budget
// is a foreign writer holding the store, so it surfaces as the terminal
// WorkspaceWriteBlockedError — not the raw transient — while still preserving the
// backend cause for diagnosis. [FRAMING:representation]
func TestRetryTransientGCContentionPromotesExhaustedManifestReadOnly(t *testing.T) {
	t.Parallel()
	// Shape the input the way production does — through wrapCommitWorkingSetError,
	// the real entry a Dolt commit error flows through — so the test is a true map
	// of the production error, not a bare-string approximation.
	results := make([]error, 0, transientRetryMaxAttempts)
	for attempt := 1; attempt <= transientRetryMaxAttempts; attempt++ {
		results = append(results, wrapCommitWorkingSetError(errors.New("Error 1105: cannot update manifest: database is read only")))
	}
	op := &fakeRetryOperation{results: results}

	err := retryTransientGCContention(
		context.Background(),
		op.run,
		noRotate,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	var blocked WorkspaceWriteBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v (%T), want WorkspaceWriteBlockedError", err, err)
	}
	if !strings.Contains(blocked.Error(), "another lit process") {
		t.Fatalf("blocked message = %q, want it to name the holder", blocked.Error())
	}
	// The backend cause chain is preserved so diagnosis still sees the transient
	// classification underneath the terminal holder error. [LAW:no-silent-failure]
	if !errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("write-blocked error dropped its transient cause chain: %v", err)
	}
	if op.calls != transientRetryMaxAttempts {
		t.Fatalf("op.calls = %d, want %d", op.calls, transientRetryMaxAttempts)
	}
}

// TestRetryTransientGCContentionKeepsExhaustedGCResetTransient guards the
// discriminator: only manifest-read-only (a foreign holder) promotes; a persistent
// online-GC reset is an intra-process condition and must stay the plain transient
// error, never the holder message.
func TestRetryTransientGCContentionKeepsExhaustedGCResetTransient(t *testing.T) {
	t.Parallel()
	results := make([]error, 0, transientRetryMaxAttempts)
	for attempt := 1; attempt <= transientRetryMaxAttempts; attempt++ {
		results = append(results, errors.New("this connection was established when this server performed an online garbage collection. please reconnect."))
	}
	op := &fakeRetryOperation{results: results}

	err := retryTransientGCContention(
		context.Background(),
		op.run,
		noRotate,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	var blocked WorkspaceWriteBlockedError
	if errors.As(err, &blocked) {
		t.Fatalf("GC-reset exhaustion wrongly promoted to WorkspaceWriteBlockedError: %v", err)
	}
	if !errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("error = %v, want ErrTransientGCContention", err)
	}
}

func TestRetryTransientGCContentionDoesNotRetryNonTransientError(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		func(context.Context) error { rotations++; return nil },
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
	t.Parallel()
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
		func(context.Context) error { return rotateErr },
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

// TestRetryTransientGCContentionCancellationEscapesBlockedRotator pins why
// connectionRotator carries ctx at all: reconnect's rotation opens a real
// engine, whose PingContext can wait out engineOpenRetryMaxElapsed against a
// held journal lock, and a cancelled mutation must escape that wait rather
// than serve it out. The rotator here blocks exactly the way that ping does —
// until its ctx dies — so the test fails (hangs past its deadline, or returns
// the wrong error) if the loop ever stops threading a live ctx through.
func TestRetryTransientGCContentionCancellationEscapesBlockedRotator(t *testing.T) {
	t.Parallel()
	op := &fakeRetryOperation{
		results: []error{
			transientGCContentionError{err: errors.New("gc reset")},
			nil,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		done <- retryTransientGCContention(
			ctx,
			op.run,
			func(rotateCtx context.Context) error { <-rotateCtx.Done(); return rotateCtx.Err() },
			func(int) time.Duration { return 0 },
			func(context.Context, time.Duration) error { return nil },
		)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("retryTransientGCContention() error = %v, want context.Canceled from the blocked rotator", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retryTransientGCContention() did not return within 5s of cancellation; the rotator is not receiving the live ctx")
	}
	if op.calls != 1 {
		t.Fatalf("op.calls = %d, want 1 (no re-attempt after a cancelled rotation)", op.calls)
	}
}

func TestTransientRetryDelayIsBounded(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	err := wrapCommitWorkingSetError(errors.New("this connection was established when this server performed an online garbage collection. this connection can no longer be used. please reconnect."))
	if !errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("errors.Is(err, ErrTransientGCContention) = false, err=%v", err)
	}
}

func TestWrapCommitWorkingSetErrorLeavesNonTransientUnmarked(t *testing.T) {
	t.Parallel()
	err := wrapCommitWorkingSetError(errors.New("permission denied"))
	if errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("errors.Is(err, ErrTransientGCContention) = true, err=%v", err)
	}
	if got, want := err.Error(), "dolt commit working set: permission denied"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestClassifyTransientGCErrorWrapsManifestReadOnly(t *testing.T) {
	t.Parallel()
	err := classifyTransientGCError(errors.New("commit add comment: Error 1105: cannot update manifest: database is read only"))
	if !errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("errors.Is(err, ErrTransientGCContention) = false, err=%v", err)
	}
}

func TestClassifyTransientGCErrorWrapsGCReset(t *testing.T) {
	t.Parallel()
	err := classifyTransientGCError(errors.New("gc_copier: this connection was established when this server performed an online garbage collection. please reconnect."))
	if !errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("errors.Is(err, ErrTransientGCContention) = false, err=%v", err)
	}
}

// TestClassifyTransientGCErrorLeavesClusterRoleReconnect guards the precision of
// the GC-reset predicate: the cluster role-transition error also says "please
// reconnect" but is not GC contention, so it must NOT be retried as transient.
func TestClassifyTransientGCErrorLeavesClusterRoleReconnect(t *testing.T) {
	t.Parallel()
	source := errors.New("this server transitioned cluster roles. this connection can no longer be used. please reconnect.")
	err := classifyTransientGCError(source)
	if errors.Is(err, ErrTransientGCContention) {
		t.Fatalf("cluster-role reconnect misclassified as GC contention: %v", err)
	}
}

func TestClassifyTransientGCErrorLeavesGenericFailures(t *testing.T) {
	t.Parallel()
	source := errors.New("permission denied")
	err := classifyTransientGCError(source)
	if err != source {
		t.Fatalf("classifyTransientGCError() = %v, want original %v", err, source)
	}
}

func TestWithCommitLockSerializesConcurrentOperations(t *testing.T) {
	// serial: no t.Parallel — asserts non-entry through a 25ms window; load-
	// sensitive.
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	s := &Store{commitLockPath: lockPath, commitLockStorageDir: storageDirOf(lockPath)}
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

// TestRetryTransientGCContentionStopsBeforeOutlastingCommitLockWaiters is the
// regression pin for the invariant Store.reconnect, this package's doc, and the
// store-operations doc all assert: the one site that takes Dolt's journal lock
// while holding the commit lock "cannot wedge" because its wait stays strictly
// inside every commit-lock waiter's budget.
//
// That was asserted in prose and enforced by nothing. The retry loop rotates
// the connection up to transientRetryMaxAttempts-1 times, and every second of
// it accrues while the commit lock is held, so the real hold is the product of
// two budgets that never referenced each other. It fit only by coincidence
// (29 x 30s = 14.5min against 15min) until links-sync-dauk derived the open
// budget from the mirror's measured hold ceiling and the product became
// 33.8min.
//
// The loop's reservation has to cover every term that runs between one check
// and the next, and there are three: the inter-attempt sleep, the new engine's
// open, and the previous engine's close. Two rows, because which omission a
// behavioural pin can SEE depends on whether the omitted term is big enough to
// cost an iteration — so each row makes one term dominant and would lose an
// iteration's worth of budget if the reservation dropped it. One row would
// pass while a term it never weighted went unreserved, which is exactly how
// the first version of this pin missed the sleep.
// [LAW:dataflow-not-control-flow] one body, the weighting is data.
//
// Not parallel: it mutates package budget variables.
func TestRetryTransientGCContentionStopsBeforeOutlastingCommitLockWaiters(t *testing.T) {
	for _, tc := range []struct {
		name      string
		open      time.Duration
		closeCost time.Duration
		delay     time.Duration
	}{
		{name: "sleep dominates the reservation", open: 100 * time.Millisecond, closeCost: 100 * time.Millisecond, delay: 450 * time.Millisecond},
		{name: "engine close dominates the reservation", open: 100 * time.Millisecond, closeCost: 450 * time.Millisecond, delay: 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreOpen := engineOpenRetryMaxElapsed
			engineOpenRetryMaxElapsed = tc.open
			t.Cleanup(func() { engineOpenRetryMaxElapsed = restoreOpen })
			restoreClose := rotationCloseReserve
			rotationCloseReserve = tc.closeCost
			t.Cleanup(func() { rotationCloseReserve = restoreClose })
			restoreAttempts := commitLockRetryAttempts
			commitLockRetryAttempts = 30
			t.Cleanup(func() { commitLockRetryAttempts = restoreAttempts })

			// A 3s budget against a 650ms reservation leaves room for four
			// rotations, so the loop stops on the hold and nowhere near its
			// 30 attempts. It lands at ~2.6s, 400ms clear of the budget —
			// margin enough to survive scheduler jitter across eight real
			// sleeps on a loaded runner, which the first version of this pin
			// (50ms of room across ten) was not.
			waiterBudget := commitLockWaiterBudget()

			// Contended forever, shaped through wrapCommitWorkingSetError so
			// this is a map of the production error rather than a bare-string
			// approximation. An operation that never clears is the sustained
			// contention the invariant is about.
			op := func(context.Context) error {
				return wrapCommitWorkingSetError(errors.New("Error 1105: cannot update manifest: database is read only"))
			}
			rotations := 0
			// The rotation really costs what reconnect costs: closing the old
			// engine plus opening the new one. Sleeping only the open would
			// leave the close out of the measured hold and the row that
			// weights it could not fail.
			rotate := func(context.Context) error {
				rotations++
				time.Sleep(engineOpenRetryMaxElapsed + rotationCloseReserve)
				return nil
			}
			// A real delay, really slept, for the same reason.
			delayForAttempt := func(int) time.Duration { return tc.delay }

			start := time.Now()
			err := retryTransientGCContention(
				context.Background(),
				op,
				rotate,
				delayForAttempt,
				func(_ context.Context, d time.Duration) error {
					time.Sleep(d)
					return nil
				},
			)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("retryTransientGCContention() error = nil, want the exhausted-contention error")
			}
			var blocked WorkspaceWriteBlockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("retryTransientGCContention() error = %v, want a WorkspaceWriteBlockedError; giving up on the hold must fail the same way giving up on the attempts does", err)
			}
			if elapsed >= waiterBudget {
				t.Fatalf("the retry held for %s against a commitLockWaiterBudget of %s; a commit-lock waiter arriving behind this holder fails with the workspace-busy sentinel naming a holder that was never wedged", elapsed, waiterBudget)
			}
			if rotations >= transientRetryMaxAttempts-1 {
				t.Fatalf("rotations = %d, want fewer than the %d the attempt count alone allows; the loop ran its attempts out instead of stopping on the hold budget, so nothing is bounding the product of the two budgets", rotations, transientRetryMaxAttempts-1)
			}
		})
	}
}
