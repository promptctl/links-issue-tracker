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
	"slices"
	"sync"
	"syscall"
	"time"
)

// DefaultGrace bounds how long the clean cancellation path may run before the
// process is hard-exited. It is chosen shorter than the receive worker's own
// fetch budget (so a SIGTERM ends that worker faster than merely letting its
// timeout lapse) and shorter than a typical supervisor's SIGKILL deadline (Docker's 10s,
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
// returned within grace, after running what the guarded work registered with
// BeforeHardExit.
//
// stop() must be called on the normal exit path; it disarms the handler and
// releases the internal goroutine. It is a no-op if an interrupt already fired.
func Guard(parent context.Context, grace time.Duration) (context.Context, func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, interruptSignals...)
	hooks := &hardExitHooks{}
	ctx, cancel := context.WithCancel(context.WithValue(parent, hardExitHooksKey{}, hooks))
	done := make(chan struct{})

	go watch(sigs, done, cancel, func() { signal.Stop(sigs) }, grace,
		escalation(hooks, hardExitHookBudget, os.Exit))

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

// hardExitHookBudget bounds how long the hooks registered with BeforeHardExit
// may run. The hard exit exists so the process ends, and a hook stuck on a
// hung filesystem must not undo that: past the budget the exit goes ahead.
const hardExitHookBudget = time.Second

type hardExitHooksKey struct{}

// hardExitHooks is what a guarded context's work asked to have done before a
// hard exit, which skips every deferred call and so every ordinary way of
// leaving a last word.
type hardExitHooks struct {
	mu  sync.Mutex
	fns []func(code int)
}

// BeforeHardExit registers fn to run, with the exit code, before the guard
// hard-exits the process. A context no Guard derived is never hard-exited, so
// there is no exit for fn to precede and nothing is registered.
func BeforeHardExit(ctx context.Context, fn func(code int)) {
	hooks, guarded := ctx.Value(hardExitHooksKey{}).(*hardExitHooks)
	if !guarded {
		return
	}
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.fns = append(hooks.fns, fn)
}

// run calls every registered hook in order, waiting at most budget for them.
func (h *hardExitHooks) run(code int, budget time.Duration) {
	h.mu.Lock()
	fns := slices.Clone(h.fns)
	h.mu.Unlock()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for _, fn := range fns {
			fn(code)
		}
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
	}
}

// escalation is the guard's hard exit: the registered hooks, within their
// budget, then exit with the conventional code for the signal.
func escalation(hooks *hardExitHooks, budget time.Duration, exit func(int)) func(os.Signal) {
	return func(sig os.Signal) {
		code := exitCode(sig)
		hooks.run(code, budget)
		exit(code)
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
	// Clean-exit vs. hard-exit is decided in THIS goroutine's single select, not a
	// timer callback that races done: escalation and the clean-path signal are two
	// cases of one atomic choice, so a clean path that finishes before grace closes
	// done first and the select provably takes it — no check-then-act window in
	// which escalation could fire over a completed shutdown.
	// [LAW:dataflow-not-control-flow]
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		// Clean path completed within grace; let main() exit with its own code.
	case <-timer.C:
		escalate(sig)
	}
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
