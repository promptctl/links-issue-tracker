package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/deadline"
	"github.com/promptctl/links-issue-tracker/internal/dirtemplate"
	"github.com/promptctl/links-issue-tracker/internal/litbuild"
)

// refusedWorkers is the launcher every invocation a test builds starts with
// (dirInvocation): every start fails t, naming the worker it would have
// started, and fails the spawn. A test that starts real workers hands its
// invocation the real launcher itself (realWorkers), as a value, so a test
// that does not cannot start one however its runs are configured.
// [LAW:no-silent-failure]
func refusedWorkers(t testing.TB) WorkerLauncher {
	return RefuseWorkers("the test was given no real worker (realWorkers)", func(err error) { t.Errorf("%v", err) })
}

// realWorkers is the launcher that starts real detached workers: lit itself
// (realLit), a process of its own.
//
// The first call builds lit, which takes minutes, so a test calls it at its
// top, before anything it times: a build inside a measured span is a verdict
// the machine gave. [LAW:no-ambient-temporal-coupling]
func realWorkers(t testing.TB) WorkerLauncher {
	t.Helper()
	return LaunchWorkersAs(realLit(t))
}

// builtLit is lit itself, cmd/lit, built at most once per test binary, by the
// first test that needs it (realLit), and removed after every test is over
// (removeRealLit). [LAW:no-shared-mutable-globals] it is written only under
// the sync.Once, and read after it.
var builtLit struct {
	once   sync.Once
	base   string // the scratch base holding it; empty until built
	binary litbuild.Binary
	output strings.Builder // the build's own output, for a build that failed
	err    error
}

// realLitBuildDeadline bounds the build of the real lit: a cold build of
// cmd/lit with the race detector, on a loaded machine, is minutes.
const realLitBuildDeadline = 20 * time.Minute

// realLit is the program of lit itself, built on the first call by the one lit
// build the tests and tools share (litbuild.Build): unstamped, since a
// detached worker runs under the released-schema ceiling, raise policy and
// deadlines its spawner hands it, which are suiteenv.Of's for every run this
// suite starts. A build that failed fails t with the build's output. No setup
// reaches the build: only a test that runs a real lit builds one.
func realLit(t testing.TB) string {
	t.Helper()
	builtLit.once.Do(func() {
		builtLit.base, builtLit.err = dirtemplate.MkdirTemp()
		if builtLit.err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), realLitBuildDeadline)
		defer cancel()
		builtLit.binary, builtLit.err = litbuild.Build(ctx, builtLit.base, &builtLit.output, raceEnabled)
	})
	if builtLit.err != nil {
		t.Fatalf("build the real lit: %v\n%s", builtLit.err, builtLit.output.String())
	}
	return builtLit.binary.Path()
}

// removeRealLit removes the real lit, if a test built it. TestMain runs it
// after every test is over; a later run's sweep removes a base this binary
// died holding.
func removeRealLit() error {
	if builtLit.base == "" {
		return nil
	}
	return os.RemoveAll(builtLit.base)
}

// exitedSpawner is a spawner that has already exited, for a worker a test
// starts directly: such a worker is the test process's own child, so the
// spawner it waits out must be a process that is not its parent, and one that
// has exited is waited out at once. It is parsed from its pid, as --parent-pid
// carries every spawner.
func exitedSpawner(t testing.TB) spawner {
	t.Helper()
	exited := exec.Command("git", "--version")
	if err := exited.Run(); err != nil {
		t.Fatalf("run a process to name as the exited spawner: %v", err)
	}
	from, err := parseSpawner(strconv.Itoa(exited.Process.Pid))
	if err != nil {
		t.Fatalf("parseSpawner() error = %v", err)
	}
	return from
}

// workerRun is the invocation a test hands a function of the run that it
// calls directly, with stderr as the run's: t's environment and the refusing
// workers, as dirInvocation hands Run.
func workerRun(t testing.TB, stderr io.Writer) Invocation {
	return dirInvocation(t, t.TempDir(), io.Discard, stderr, nil)
}

// An invocation built without a worker launcher, or with an environment that
// carries no deadlines, is refused, where the same invocation given both runs.
func TestRunRefusesAnInvocationGivenNoWorkersOrDeadlines(t *testing.T) {
	t.Parallel()
	inv := dirInvocation(t, t.TempDir(), io.Discard, io.Discard, []string{"version"})
	if err := Run(context.Background(), inv); err != nil {
		t.Fatalf("lit version with workers and deadlines: %v", err)
	}
	for name, drop := range map[string]func(*Invocation){
		"worker launcher": func(inv *Invocation) { inv.Workers = nil },
		"deadlines":       func(inv *Invocation) { inv.Env = inv.Env.WithDeadlines(deadline.Deadlines{}) },
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
	err := dirInvocation(rec, t.TempDir(), io.Discard, io.Discard, nil).Workers.start(detachedWorker{args: []string{backgroundMirrorSubcommand, "--store", "/s"}})
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
