package interrupt

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestWatchFirstInterruptCancels pins the clean path: the first interrupt cancels
// the context so ctx-aware work abandons and deferred cleanup runs.
func TestWatchFirstInterruptCancels(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	escalated := make(chan os.Signal, 1)
	go watch(sigs, done, cancel, func() {}, time.Hour, func(s os.Signal) { escalated <- s })

	sigs <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("first interrupt did not cancel the context")
	}
	// A long grace means escalation must NOT have fired on the clean path yet.
	select {
	case s := <-escalated:
		t.Fatalf("escalated %v before grace elapsed", s)
	default:
	}
	close(done) // let the goroutine finish
}

// TestWatchGraceEscalates pins the guarantee: after the first interrupt, if the
// clean path never completes (done never closes — a ctx-ignoring wedge), the
// grace timer hard-exits. escalate is injected so the guarantee is observable
// without terminating the test process.
func TestWatchGraceEscalates(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	defer close(done)

	escalated := make(chan os.Signal, 1)
	go watch(sigs, done, func() {}, func() {}, 10*time.Millisecond, func(s os.Signal) { escalated <- s })

	sigs <- syscall.SIGTERM
	select {
	case got := <-escalated:
		if got != syscall.SIGTERM {
			t.Fatalf("escalated with %v, want SIGTERM", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("grace elapsed but escalation never fired — a ctx-ignoring wedge would hang until SIGKILL")
	}
}

// TestWatchNoEscalateWhenDoneRacesGrace pins the escalation guarantee's other
// edge: a signal arrives (so the grace timer is running), but the clean path
// completes (done closes) before grace elapses — escalation must NOT fire,
// because main() is already exiting with the correct code. watch decides this in
// one select, so a done closed before the timer deterministically wins.
func TestWatchNoEscalateWhenDoneRacesGrace(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})

	escalated := make(chan os.Signal, 1)
	returned := make(chan struct{})
	go func() {
		watch(sigs, done, func() {}, func() {}, 30*time.Millisecond, func(s os.Signal) { escalated <- s })
		close(returned)
	}()

	sigs <- syscall.SIGTERM
	close(done) // clean path completes well before the 30ms grace
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return after done closed")
	}
	// Past grace: the guarded callback must have been a no-op.
	time.Sleep(60 * time.Millisecond)
	select {
	case s := <-escalated:
		t.Fatalf("escalated %v after the clean path completed before grace", s)
	default:
	}
}

// TestWatchRestoresDefaultDisposition pins that the OS default disposition is
// restored on the first interrupt, so a SECOND interrupt is not swallowed but
// terminates the process. This is the specific gap that makes plain
// signal.NotifyContext insufficient for this job.
func TestWatchRestoresDefaultDisposition(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	defer close(done)

	restored := make(chan struct{}, 1)
	go watch(sigs, done, func() {}, func() { restored <- struct{}{} }, time.Hour, func(os.Signal) {})

	sigs <- syscall.SIGINT
	select {
	case <-restored:
	case <-time.After(2 * time.Second):
		t.Fatal("default disposition was not restored after the first interrupt")
	}
}

// TestWatchNormalExitDoesNotEscalate pins that a normal shutdown (done closed,
// no interrupt) returns without ever arming escalation.
func TestWatchNormalExitDoesNotEscalate(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		watch(sigs, done, func() {}, func() { t.Error("restoreDefault ran on the normal exit path") }, time.Millisecond, func(os.Signal) { t.Error("escalated on the normal exit path") })
		close(returned)
	}()

	close(done)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return on normal shutdown")
	}
	// Give any errant timer time to (wrongly) fire; the escalate/restoreDefault
	// closures t.Error if they run.
	time.Sleep(20 * time.Millisecond)
}

// TestExitCode pins the 128+signum contract for the escalation exit code.
func TestExitCode(t *testing.T) {
	cases := []struct {
		sig  os.Signal
		want int
	}{
		{syscall.SIGINT, 130},
		{syscall.SIGTERM, 143},
		{os.Interrupt, 130}, // os.Interrupt == syscall.SIGINT on the tested platforms
	}
	for _, tc := range cases {
		if got := exitCode(tc.sig); got != tc.want {
			t.Errorf("exitCode(%v) = %d, want %d", tc.sig, got, tc.want)
		}
	}
	// A non-syscall signal has no numeric disposition; it falls back to a generic
	// failure code rather than a bogus 128+garbage.
	if got := exitCode(fakeSignal{}); got != 1 {
		t.Errorf("exitCode(non-syscall signal) = %d, want 1", got)
	}
}

type fakeSignal struct{}

func (fakeSignal) String() string { return "fake" }
func (fakeSignal) Signal()        {}
