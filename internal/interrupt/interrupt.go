// Package interrupt owns a process's interrupt-shutdown lifecycle: it turns an
// operating-system interrupt (SIGINT/SIGTERM) into cancellation of a context,
// and — this is the load-bearing part — guarantees the process still terminates
// even when the work in flight ignores that cancellation.
//
// The two-layer shape is deliberate. Cancelling the context is the CLEAN path:
// ctx-aware work (a Dolt fetch, a lock-acquire wait) abandons and the deferred
// cleanup that frees the embedded store's commit lock runs. But an operation
// that never consults ctx would keep the process alive indefinitely after the
// cancel — reachable only by SIGKILL, which is exactly the wedge this package
// exists to prevent (a SIGTERM that "removed the commit lock but the process
// still did not exit"). So cancellation alone is never trusted: a grace timer
// hard-exits, and the OS default disposition is restored so a second interrupt
// terminates at once. [LAW:no-ambient-temporal-coupling] the shutdown ordering
// has one explicit owner here rather than living in incidental signal defaults.
package interrupt

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// DefaultGrace bounds how long the clean cancellation path may run before the
// process is hard-exited. It is chosen shorter than the inline receive's own
// fetch budget (so a SIGTERM exits faster than merely letting that timeout
// lapse) and shorter than a typical supervisor's SIGKILL deadline (Docker's 10s,
// Kubernetes' 30s), so lit exits on its own terms before it is force-killed. On
// the common path — work that honors ctx — the process exits in milliseconds and
// this bound is never reached.
const DefaultGrace = 5 * time.Second

// interruptSignals is the graceful-shutdown signal set: an interactive Ctrl-C
// (SIGINT) and a supervisor's terminate (SIGTERM). Both mean "stop now, cleanly
// if you can". SIGKILL is deliberately absent — it cannot be caught, and the
// escalation path exists precisely so the process never REQUIRES it.
var interruptSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

// Guard installs interrupt→cancel over parent and returns the derived context
// plus a stop func the entrypoint defers for the normal (no-signal) exit path.
//
// On the first interrupt it cancels the context so ctx-aware work abandons and
// deferred cleanup runs, then arms escalation so a ctx-ignoring operation can
// never leave the process terminable only by SIGKILL: the OS default disposition
// is restored (a second interrupt terminates immediately) AND a grace timer
// hard-exits with the conventional 128+signum code if the clean path has not
// returned within grace.
//
// stop() must be called on the normal exit path; it disarms the handler and
// releases the internal goroutine. It is a no-op if an interrupt already fired.
func Guard(parent context.Context, grace time.Duration) (context.Context, func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, interruptSignals...)
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})

	go watch(sigs, done, cancel, func() { signal.Stop(sigs) }, grace,
		func(sig os.Signal) { os.Exit(exitCode(sig)) })

	var stopped bool
	return ctx, func() {
		if stopped {
			return
		}
		stopped = true
		signal.Stop(sigs)
		cancel()
		close(done)
	}
}

// watch drives the interrupt lifecycle, isolated from OS wiring (signal.Stop,
// os.Exit) so the escalation contract is unit-testable without real signals or
// killing the test process. It blocks until either normal shutdown (done) or the
// first interrupt (sigs); on the interrupt it cancels, restores the default
// disposition, and arms the grace-timer escalation, then waits for the clean
// path to finish (which stops the timer).
//
// The select on two channels is genuine event multiplexing, not avoidable
// branching — which event fired IS the state. [LAW:dataflow-not-control-flow]
// (the last-inch concurrency exception).
func watch(
	sigs <-chan os.Signal,
	done <-chan struct{},
	cancel context.CancelFunc,
	restoreDefault func(),
	grace time.Duration,
	escalate func(os.Signal),
) {
	var sig os.Signal
	select {
	case <-done:
		// Normal shutdown: no interrupt ever arrived, nothing to escalate.
		return
	case sig = <-sigs:
	}

	// First interrupt: begin the clean path.
	cancel()
	// Revert to the OS default disposition so a SECOND interrupt terminates the
	// process immediately instead of being swallowed here — the specific trap
	// that makes signal.NotifyContext insufficient for this job.
	restoreDefault()
	// Guarantee termination even if no second interrupt ever comes: a ctx-ignoring
	// wedge is hard-exited once grace elapses. This is the guarantee; the cancel
	// above is only best-effort.
	//
	// The callback is guarded against a clean path that finishes right at grace:
	// once done is closed, main() is already exiting with the correct code, so a
	// timer that fires in the narrow window before `defer timer.Stop()` runs must
	// not hard-exit over it. Escalation therefore fires only while the clean path
	// is still demonstrably outstanding — precisely its contract.
	timer := time.AfterFunc(grace, func() {
		select {
		case <-done:
		default:
			escalate(sig)
		}
	})
	defer timer.Stop()
	<-done
}

// exitCode maps an interrupt signal to the conventional 128+signum shell exit
// code, so a process terminated by escalation reports the same code a shell sees
// from an uncaught signal (SIGINT→130, SIGTERM→143). [CLI] exit codes are a
// contract.
func exitCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}
