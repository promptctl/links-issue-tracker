package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dirtemplate"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// realWorkers holds the launcher of each test that starts real detached
// workers, keyed by test name, as runEnvs holds each test's environment.
var (
	realWorkersMu sync.Mutex
	realWorkers   = map[string]WorkerLauncher{}
)

// useRealWorkers hands every run t and its subtests start the launcher that
// starts real detached workers: lit itself (realLit), a process of its own.
// It is the one way a test's runs reach one, so a test that never calls it
// cannot start a worker however its runs are configured.
//
// The first call builds lit, which takes minutes, so a test calls it at its
// top, before anything it times: a build inside a measured span is a
// verdict the machine gave. [LAW:no-ambient-temporal-coupling]
func useRealWorkers(t testing.TB) {
	t.Helper()
	program := realLit(t)
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
	realWorkers[name] = LaunchWorkersAs(program, nil)
}

// builtLit is lit itself, cmd/lit, built at most once per test binary, by the
// first test that needs it (realLit), and removed after every test is over
// (removeRealLit). [LAW:no-shared-mutable-globals] it is written only under
// the sync.Once, and read after it.
var builtLit struct {
	once sync.Once
	base string // the scratch base holding it; empty until built
	path string
	err  error
}

// realLitBuildDeadline bounds the build of the real lit: a cold build of
// cmd/lit with the race detector, on a loaded machine, is minutes.
const realLitBuildDeadline = 20 * time.Minute

// realLit is the program of lit itself, built on the first call and stamped
// as a release of this tree is (the schema it carries, raising as a release
// does), so its runs read what suiteenv.Of hands this binary's. A build that
// failed fails t with the build's output. No setup reaches the build: only a
// test that runs a real lit builds one. [LAW:one-source-of-truth]
func realLit(t testing.TB) string {
	t.Helper()
	builtLit.once.Do(func() { builtLit.base, builtLit.path, builtLit.err = buildRealLit() })
	if builtLit.err != nil {
		t.Fatalf("build the real lit: %v", builtLit.err)
	}
	return builtLit.path
}

// buildRealLit builds cmd/lit into a scratch base a later run's sweep
// removes if this binary dies holding it; a failed build removes its own.
func buildRealLit() (base, program string, err error) {
	root, err := moduleRoot()
	if err != nil {
		return "", "", err
	}
	base, err = dirtemplate.MkdirTemp()
	if err != nil {
		return "", "", err
	}
	versionPkg := reflect.TypeOf(version.SchemaCeiling{}).PkgPath()
	ldflags := "-X " + versionPkg + ".ReleasedSchema=" + version.ReleasedSchemaCarried + " -X " + versionPkg + ".Origin=" + version.OriginRelease
	args := []string{"build", "-buildvcs=false", "-ldflags", ldflags}
	if raceEnabled {
		args = append(args, "-race")
	}
	// An -o ending in a separator is a directory go build makes and names the
	// program in: "lit", which os/exec finds again with the suffix Windows
	// wants.
	args = append(args, "-o", base+string(filepath.Separator), "./cmd/lit")
	ctx, cancel := context.WithTimeout(context.Background(), realLitBuildDeadline)
	defer cancel()
	// The program is named at the spawn, so what the build starts is go, in
	// this process's own environment, and nothing a caller could hand it.
	// [LAW:types-are-the-program]
	build := exec.CommandContext(ctx, "go", args...)
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		return "", "", errors.Join(fmt.Errorf("go %v in %s: %w\n%s", args, root, err, out), os.RemoveAll(base))
	}
	return base, filepath.Join(base, "lit"), nil
}

// removeRealLit removes the real lit, if a test built it. TestMain runs it
// after every test is over.
func removeRealLit() error {
	if builtLit.base == "" {
		return nil
	}
	return os.RemoveAll(builtLit.base)
}

// moduleRoot is the directory of the go.mod above this file, as it was
// compiled.
func moduleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return "", fmt.Errorf("this test binary does not know where its source is (%q), so it cannot build cmd/lit", file)
	}
	for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("look for go.mod in %s: %w", dir, err)
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("no go.mod above %s", file)
		}
	}
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
