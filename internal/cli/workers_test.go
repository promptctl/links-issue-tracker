package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

// realWorkers holds the launcher of each test that starts real detached
// workers, keyed by test name, as runEnvs holds each test's environment.
var (
	realWorkersMu sync.Mutex
	realWorkers   = map[string]WorkerLauncher{}
)

// useRealWorkers hands every run t and its subtests start the launcher that
// starts real detached workers: lit itself (suiteLit), a process of its own.
// It is the one way a test's runs reach one, so a test that never calls it
// cannot start a worker however its runs are configured.
func useRealWorkers(t testing.TB) {
	t.Helper()
	name := t.Name()
	realWorkersMu.Lock()
	defer realWorkersMu.Unlock()
	if _, ok := realWorkers[name]; !ok {
		t.Cleanup(func() {
			realWorkersMu.Lock()
			defer realWorkersMu.Unlock()
			delete(realWorkers, name)
		})
	}
	realWorkers[name] = LaunchWorkersAs(suiteLitPath())
}

// runWorkers is the launcher t's runs are handed: the real one when t or a
// test enclosing it asked for it (useRealWorkers), and otherwise one that
// refuses every start, failing t.
func runWorkers(t testing.TB) WorkerLauncher {
	parts := strings.Split(t.Name(), "/")
	realWorkersMu.Lock()
	defer realWorkersMu.Unlock()
	for i := len(parts); i > 0; i-- {
		if launcher, ok := realWorkers[strings.Join(parts[:i], "/")]; ok {
			return launcher
		}
	}
	return refusedWorkers{t: t}
}

// refusedWorkers is the launcher of a test that was given no real worker:
// every start fails the test, naming the worker it would have started, and
// fails the spawn. [LAW:no-silent-failure]
type refusedWorkers struct{ t testing.TB }

func (r refusedWorkers) start(w detachedWorker) error {
	err := fmt.Errorf("the test was given no real worker (useRealWorkers), so lit %s was not started", strings.Join(w.args, " "))
	r.t.Errorf("%v", err)
	return err
}

// workerRun is the invocation a test hands a function of the run that it
// calls directly, with stderr as the run's: t's environment, workers and
// deadlines, as dirInvocation hands Run.
func workerRun(t testing.TB, stderr io.Writer) Invocation {
	return dirInvocation(t, t.TempDir(), io.Discard, stderr, nil)
}

// A literal that leaves the worker launcher out holds a nil one, and one that
// leaves the deadlines out holds none. Run refuses each, where the same
// invocation given both runs.
func TestRunRefusesAnInvocationGivenNoWorkersOrDeadlines(t *testing.T) {
	t.Parallel()
	inv := dirInvocation(t, t.TempDir(), io.Discard, io.Discard, []string{"version"})
	if err := Run(context.Background(), inv); err != nil {
		t.Fatalf("lit version with workers and deadlines: %v", err)
	}
	for name, drop := range map[string]func(*Invocation){
		"worker launcher": func(inv *Invocation) { inv.Workers = nil },
		"deadlines":       func(inv *Invocation) { inv.Deadlines = store.Deadlines{} },
	} {
		given := inv
		drop(&given)
		if err := Run(context.Background(), given); err == nil || err.Error() != "the invocation was given no "+name {
			t.Errorf("lit version with no %s = %v, want it refused as given none", name, err)
		}
	}
}

// A test given no real worker that starts one anyway fails, and the spawn
// fails with it, naming the worker.
func TestATestGivenNoRealWorkerFailsWhenItStartsOne(t *testing.T) {
	t.Parallel()
	rec := &errorRecorder{TB: t}
	err := runWorkers(rec).start(detachedWorker{args: []string{backgroundMirrorSubcommand, "--store", "/s"}})
	if err == nil || !strings.Contains(err.Error(), "given no real worker") || !strings.Contains(err.Error(), backgroundMirrorSubcommand) {
		t.Fatalf("start() = %v; want it refused, naming the worker", err)
	}
	if rec.failure == nil || rec.failure.Error() != err.Error() {
		t.Fatalf("the test was failed with %v; want %v", rec.failure, err)
	}
}

// errorRecorder records the Errorf a test helper fails its test with, without
// failing the test it wraps.
type errorRecorder struct {
	testing.TB
	failure error
}

func (r *errorRecorder) Errorf(format string, args ...any) {
	r.failure = fmt.Errorf(format, args...)
}
