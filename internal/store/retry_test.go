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

// heldCommitLock marks ctx the way acquireCommitLock does, as a hold that began
// `ago` before now, so the retry can be called without a real lock file.
func heldCommitLock(ctx context.Context, ago time.Duration) context.Context {
	return context.WithValue(ctx, commitLockContextKey{}, time.Now().Add(-ago))
}

func TestRetryTransientGCContentionRetriesTransientError(t *testing.T) {
	t.Parallel()
	op := &fakeRetryOperation{
		results: []error{
			transientGCContentionError{err: errors.New("transient manifest read only")},
			nil,
		},
	}

	err := retryTransientGCContention(
		heldCommitLock(context.Background(), 0),
		op.run,
		noRotate,
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
	lastErr := transientGCContentionError{err: errors.New("transient final")}
	op := &fakeRetryOperation{results: []error{
		transientGCContentionError{err: errors.New("transient")},
		lastErr,
	}}

	err := retryTransientGCContention(
		heldCommitLock(context.Background(), 0),
		op.run,
		noRotate,
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
	if op.calls != 2 {
		t.Fatalf("op.calls = %d, want 2 (the first run and one retry on the rotated connection)", op.calls)
	}
}

// TestRetryTransientGCContentionPromotesExhaustedManifestReadOnly pins that a
// manifest-read-only that survives the entire retry budget is a foreign writer
// holding the store, so it surfaces as the terminal WorkspaceWriteBlockedError
// — not the raw transient — while still preserving the backend cause for
// diagnosis. [FRAMING:representation]
func TestRetryTransientGCContentionPromotesExhaustedManifestReadOnly(t *testing.T) {
	t.Parallel()
	// Shape the input the way production does — through wrapCommitWorkingSetError,
	// the real entry a Dolt commit error flows through — so the test is a true map
	// of the production error, not a bare-string approximation.
	readOnly := func() error {
		return wrapCommitWorkingSetError(errors.New("Error 1105: cannot update manifest: database is read only"))
	}
	op := &fakeRetryOperation{results: []error{readOnly(), readOnly()}}

	err := retryTransientGCContention(
		heldCommitLock(context.Background(), 0),
		op.run,
		noRotate,
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
	if op.calls != 2 {
		t.Fatalf("op.calls = %d, want 2", op.calls)
	}
}

// TestRetryTransientGCContentionKeepsExhaustedGCResetTransient guards the
// discriminator: only manifest-read-only (a foreign holder) promotes; a persistent
// online-GC reset is an intra-process condition and must stay the plain transient
// error, never the holder message.
func TestRetryTransientGCContentionKeepsExhaustedGCResetTransient(t *testing.T) {
	t.Parallel()
	gcReset := errors.New("this connection was established when this server performed an online garbage collection. please reconnect.")
	op := &fakeRetryOperation{results: []error{gcReset, gcReset}}

	err := retryTransientGCContention(
		heldCommitLock(context.Background(), 0),
		op.run,
		noRotate,
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
		heldCommitLock(context.Background(), 0),
		op.run,
		noRotate,
	)
	if err == nil {
		t.Fatal("retryTransientGCContention() error = nil, want non-nil")
	}
	if op.calls != 1 {
		t.Fatalf("op.calls = %d, want 1", op.calls)
	}
}

// TestRetryTransientGCContentionRotatesConnectionOnceBeforeTheRetry pins
// that the GC reset poisons the connection, so the retry must rotate it before
// running again — once, and never after the succeeding call.
func TestRetryTransientGCContentionRotatesConnectionOnceBeforeTheRetry(t *testing.T) {
	t.Parallel()
	op := &fakeRetryOperation{
		results: []error{
			transientGCContentionError{err: errors.New("gc reset")},
			nil,
		},
	}
	rotations := 0

	err := retryTransientGCContention(
		heldCommitLock(context.Background(), 0),
		op.run,
		func(context.Context) error { rotations++; return nil },
	)
	if err != nil {
		t.Fatalf("retryTransientGCContention() error = %v", err)
	}
	if op.calls != 2 {
		t.Fatalf("op.calls = %d, want 2", op.calls)
	}
	if rotations != 1 {
		t.Fatalf("rotations = %d, want 1 (before the retry, none after success)", rotations)
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
		heldCommitLock(context.Background(), 0),
		op.run,
		func(context.Context) error { return rotateErr },
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
// engine, whose PingContext can wait out coResidentHolderWait against a
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
	ctx, cancel := context.WithCancel(heldCommitLock(context.Background(), 0))
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

// TestWrapCommitWorkingSetErrorMarksGCReset covers the variant: a commit that
// hits Dolt's online-GC connection invalidation must be classified transient so
// it is retried (with a reconnect), not surfaced raw.
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
// Everything the check reserves is weighted by some row, and each refusing row
// is sized so that dropping the term it weights lets the rotation through and
// carries the hold past the budget: the new engine's open, the old engine's
// close, the second run (projected at the first run's cost), and the hold the
// caller had already spent before the retry was called. The last row is the
// other side of the same check: a hold with room left gets its rotation.
// [LAW:dataflow-not-control-flow] one body, the weighting is data.
//
// Not parallel: it mutates package budget variables.
func TestRetryTransientGCContentionStopsBeforeOutlastingCommitLockWaiters(t *testing.T) {
	for _, tc := range []struct {
		name          string
		open          time.Duration
		closeCost     time.Duration
		heldBefore    time.Duration
		work          time.Duration
		wantRotations int
	}{
		// 160+160+400 >= 700; without the close 620, without the open 420,
		// without the second run 560: each lets a 720ms hold through.
		{name: "engine open and second run weighted", open: 300 * time.Millisecond, closeCost: 100 * time.Millisecond, work: 160 * time.Millisecond, wantRotations: 0},
		// 70+70+550 >= 650; without the close 240, without the open 590.
		{name: "engine close weighted", open: 100 * time.Millisecond, closeCost: 450 * time.Millisecond, work: 70 * time.Millisecond, wantRotations: 0},
		// 350+0+400 >= 700; counted from the call instead, 400 lets a 750ms
		// hold through.
		{name: "hold spent before the retry weighted", open: 300 * time.Millisecond, closeCost: 100 * time.Millisecond, heldBefore: 350 * time.Millisecond, wantRotations: 0},
		// 50+50+400 < 700: the rotation lands and the hold ends near 500ms.
		{name: "a hold with room left gets its rotation", open: 300 * time.Millisecond, closeCost: 100 * time.Millisecond, work: 50 * time.Millisecond, wantRotations: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreOpen := coResidentHolderWait
			coResidentHolderWait = tc.open
			t.Cleanup(func() { coResidentHolderWait = restoreOpen })
			restoreClose := rotationCloseReserve
			rotationCloseReserve = tc.closeCost
			t.Cleanup(func() { rotationCloseReserve = restoreClose })
			waiterBudget := commitLockWaiterBudget()

			// Contended forever, shaped through wrapCommitWorkingSetError so
			// this is a map of the production error rather than a bare-string
			// approximation. Every run costs the same, as a re-run of one
			// operation does.
			op := func(context.Context) error {
				time.Sleep(tc.work)
				return wrapCommitWorkingSetError(errors.New("Error 1105: cannot update manifest: database is read only"))
			}
			rotations := 0
			// The rotation really costs what reconnect costs: closing the old
			// engine plus opening the new one. Sleeping only the open would
			// leave the close out of the measured hold and the row that
			// weights it could not fail.
			rotate := func(context.Context) error {
				rotations++
				time.Sleep(coResidentHolderWait + rotationCloseReserve)
				return nil
			}

			ctx := heldCommitLock(context.Background(), tc.heldBefore)
			heldSince, _ := commitLockHeldSince(ctx)
			err := retryTransientGCContention(ctx, op, rotate)
			hold := time.Since(heldSince)

			var blocked WorkspaceWriteBlockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("retryTransientGCContention() error = %v, want a WorkspaceWriteBlockedError; a refused rotation must fail the same way a failed retry does", err)
			}
			if rotations != tc.wantRotations {
				t.Fatalf("rotations = %d, want %d", rotations, tc.wantRotations)
			}
			if hold >= waiterBudget {
				t.Fatalf("the commit lock was held for %s against a commitLockWaiterBudget of %s; a commit-lock waiter arriving behind this holder fails with the workspace-busy sentinel naming a holder that was never wedged", hold, waiterBudget)
			}
		})
	}
}

// TestRetryTransientGCContentionRefusesAContextWithoutTheCommitLock pins the
// precondition the rotation depends on: the retry swaps the store's
// connection, which only the commit lock's holder may do, and it budgets from
// when that lock was taken, so a context without the lock's marker is refused
// before the operation runs.
func TestRetryTransientGCContentionRefusesAContextWithoutTheCommitLock(t *testing.T) {
	t.Parallel()
	op := &fakeRetryOperation{results: []error{nil}}
	err := retryTransientGCContention(context.Background(), op.run, noRotate)
	if err == nil || !strings.Contains(err.Error(), "without the commit lock held") {
		t.Fatalf("retryTransientGCContention() error = %v, want the missing-lock refusal", err)
	}
	if op.calls != 0 {
		t.Fatalf("op.calls = %d, want 0", op.calls)
	}
}

// TestRetryTransientGCContentionAnnouncesTheRotation pins that a write which
// recovered on a rotated connection says so on the operator channel, naming
// the failure that caused it. [LAW:nothing-unseen]
//
// Not parallel: it swaps the package's notice writer.
func TestRetryTransientGCContentionAnnouncesTheRotation(t *testing.T) {
	var notices strings.Builder
	restore := lockWaitNoticeWriter
	lockWaitNoticeWriter = &notices
	t.Cleanup(func() { lockWaitNoticeWriter = restore })

	op := &fakeRetryOperation{results: []error{
		transientGCContentionError{err: errors.New("gc reset seen by this connection")},
		nil,
	}}
	if err := retryTransientGCContention(heldCommitLock(context.Background(), 0), op.run, noRotate); err != nil {
		t.Fatalf("retryTransientGCContention() error = %v", err)
	}
	if got := notices.String(); !strings.Contains(got, "gc reset seen by this connection") || !strings.Contains(got, "retrying once") {
		t.Fatalf("notice = %q, want the failure and the retry named", got)
	}
}
