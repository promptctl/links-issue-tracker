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
	"maps"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/dolthub/dolt/go/libraries/utils/gitcmd"
)

// DefaultGrace bounds how long the clean cancellation path may run before the
// process is hard-exited. It gives a cancelled git gitcmd.CancelBound to end
// and reach the invocation's event, and a second for the command to unwind
// and write it, so a hard exit never records a cancel still settling as none.
// It is shorter than the receive worker's own fetch budget (so a SIGTERM ends
// that worker faster than merely letting its timeout lapse) and, with the
// hard-exit hooks' budget, shorter than a typical supervisor's SIGKILL
// deadline (Docker's 10s, Kubernetes' 30s), so lit exits on its own terms
// before it is force-killed. On the common path — work that honors ctx — the
// process exits in milliseconds and this bound is never reached.
// [LAW:no-ambient-temporal-coupling]
const DefaultGrace = gitcmd.CancelBound + time.Second

// interruptSignals is the graceful-shutdown signal set: an interactive Ctrl-C
// (SIGINT) and a supervisor's terminate (SIGTERM). Both mean "stop now, cleanly
// if you can". SIGKILL is deliberately absent — it cannot be caught, and the
// escalation path exists precisely so the process never REQUIRES it. Each is
// keyed to its conventional name, which is how a hard exit reports it.
// [LAW:one-source-of-truth] the set caught and the names reported are one table.
var interruptSignals = map[os.Signal]string{syscall.SIGINT: "SIGINT", syscall.SIGTERM: "SIGTERM"}

// Guard installs interrupt→cancel over parent and returns the derived context
// plus the exit func the entrypoint ends the process through, on every path.
//
// On the first interrupt it cancels the context so ctx-aware work abandons and
// deferred cleanup runs, then arms escalation so a ctx-ignoring operation can
// never leave the process terminable only by SIGKILL: the OS default disposition
// is restored (a second interrupt terminates immediately) AND a grace timer
// hard-exits if the clean path has not returned within grace, after running
// what the guarded work registered with BeforeHardExit.
//
// The process's exit code is decided once, here: by Claim (or exit) on the
// clean path, or by the hard exit with the conventional 128+signum code, which
// ever comes first. A hard exit after a clean claim exits with the claimed
// code; an exit after a hard claim waits for the hard exit to end the process.
// [LAW:single-enforcer]
func Guard(parent context.Context, grace time.Duration) (context.Context, func(code int)) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, slices.Collect(maps.Keys(interruptSignals))...)
	g := &guard{}
	ctx, cancel := context.WithCancel(context.WithValue(parent, guardKey{}, g))
	done := make(chan struct{})

	go watch(sigs, done, cancel, func() { signal.Stop(sigs) }, grace,
		escalation(g, hardExitHookBudget, os.Exit))

	return ctx, func(code int) {
		decided := g.decision.claim(Decision{Code: code})
		if decided.Hard() {
			// The hard exit holds the claim and is already ending the process.
			select {}
		}
		signal.Stop(sigs)
		cancel()
		close(done)
		os.Exit(decided.Code)
	}
}

type guardKey struct{}

// guard is what a Guard-derived context carries: the hooks its work
// registered, and the process's exit decision. One value, so a context has
// both or neither.
type guard struct {
	hooks    hardExitHooks
	decision exitDecision
}

func guardOf(ctx context.Context) (*guard, bool) {
	g, ok := ctx.Value(guardKey{}).(*guard)
	return g, ok
}

// Decision is the process's exit: the code it exits with and, when the hard
// exit decided it, the interrupt whose grace ran out.
type Decision struct {
	Code   int
	Signal string
}

// Hard reports whether the hard exit decided the process's exit.
func (d Decision) Hard() bool { return d.Signal != "" }

// exitDecision is the process's exit, decided once by whichever of the clean
// path and the hard exit claims it first.
type exitDecision struct {
	mu      sync.Mutex
	decided *Decision
}

// claim decides the exit as want unless it is already decided, and returns
// the decision that stands.
func (d *exitDecision) claim(want Decision) Decision {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.decided == nil {
		d.decided = &want
	}
	return *d.decided
}

// Claim claims the process's exit with code for the clean end of the guarded
// work, before that end is recorded or printed, so what is recorded and
// printed is what the process exits with. It returns the decision that
// stands: code, a clean claim made earlier, or — when the grace already ran
// out — the hard exit's. A context no Guard derived is never hard-exited, and
// its claim is code.
func Claim(ctx context.Context, code int) Decision {
	g, guarded := guardOf(ctx)
	if !guarded {
		return Decision{Code: code}
	}
	return g.decision.claim(Decision{Code: code})
}

// hardExitHookBudget bounds how long the hooks registered with BeforeHardExit
// may run. The hard exit exists so the process ends, and a hook stuck on a
// hung filesystem must not undo that: past the budget the exit goes ahead.
const hardExitHookBudget = time.Second

// hardExitHooks is what a guarded context's work asked to have done before a
// hard exit, which skips every deferred call and so every ordinary way of
// leaving a last word.
type hardExitHooks struct {
	mu  sync.Mutex
	fns []func(signal string, code int)
}

// BeforeHardExit registers fn to run, with the name of the interrupt that
// started the shutdown (SIGINT, SIGTERM) and the exit code, before the guard
// hard-exits the process. A context no Guard derived is never hard-exited, so
// there is no exit for fn to precede and nothing is registered.
func BeforeHardExit(ctx context.Context, fn func(signal string, code int)) {
	g, guarded := guardOf(ctx)
	if !guarded {
		return
	}
	g.hooks.mu.Lock()
	defer g.hooks.mu.Unlock()
	g.hooks.fns = append(g.hooks.fns, fn)
}

// run calls every registered hook in order, waiting at most budget for them.
func (h *hardExitHooks) run(signal string, code int, budget time.Duration) {
	h.mu.Lock()
	fns := slices.Clone(h.fns)
	h.mu.Unlock()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for _, fn := range fns {
			fn(signal, code)
		}
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
	}
}

// escalation is the guard's hard exit: claim the exit, run the registered
// hooks within their budget, then exit with the claimed code — the
// conventional code for the signal, or the clean end's when it claimed first.
func escalation(g *guard, budget time.Duration, exit func(int)) func(os.Signal) {
	return func(sig os.Signal) {
		decided := g.decision.claim(Decision{Code: exitCode(sig), Signal: interruptSignals[sig]})
		g.hooks.run(interruptSignals[sig], decided.Code, budget)
		exit(decided.Code)
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
