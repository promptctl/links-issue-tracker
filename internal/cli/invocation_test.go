package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// dirInvocation is the invocation a test runs lit with in dir. stdin is empty,
// as /dev/null is to a `go test` process, and its environment is the test
// process's (testEnv).
func dirInvocation(dir string, stdout, stderr io.Writer, args []string) Invocation {
	return Invocation{Dir: WorkingDirOf(dir, nil), Args: args, Env: testEnv(), Stdin: strings.NewReader(""), Stdout: stdout, Stderr: stderr}
}

// testEnv is the test process's environment, read when it is called, so a
// test's t.Setenv reaches the run it starts; links-testperf-xxsx.2.1 hands
// those tests an environment of their own instead.
func testEnv() environ.Env { return environ.Of(os.Environ()) }

// A literal that leaves Env out holds the zero Env. Run refuses it, where the
// same invocation given an environment runs.
func TestRunRefusesAnInvocationGivenNoEnvironment(t *testing.T) {
	t.Parallel()
	inv := dirInvocation(t.TempDir(), io.Discard, io.Discard, []string{"version"})
	if err := Run(context.Background(), inv); err != nil {
		t.Fatalf("lit version with an environment: %v", err)
	}
	inv.Env = environ.Env{}
	if err := Run(context.Background(), inv); err == nil || err.Error() != "the invocation was given no environment" {
		t.Fatalf("lit version with no environment = %v, want it refused as given none", err)
	}
}

// A literal that leaves a standard stream out holds a nil one. Run refuses it
// before any command can write to or read from it.
func TestRunRefusesAnInvocationGivenNoStream(t *testing.T) {
	t.Parallel()
	for name, drop := range map[string]func(*Invocation){
		"stdin":  func(inv *Invocation) { inv.Stdin = nil },
		"stdout": func(inv *Invocation) { inv.Stdout = nil },
		"stderr": func(inv *Invocation) { inv.Stderr = nil },
	} {
		inv := dirInvocation(t.TempDir(), io.Discard, io.Discard, []string{"version"})
		drop(&inv)
		if err := Run(context.Background(), inv); err == nil || err.Error() != "the invocation was given no "+name {
			t.Errorf("lit version with no %s = %v, want it refused as given none", name, err)
		}
	}
}

// cwdInvocation is dirInvocation for a test that steers Run by changing the
// process's directory; links-testperf-xxsx.2.1 hands those tests their
// directory instead.
func cwdInvocation(stdout, stderr io.Writer, args []string) Invocation {
	inv := dirInvocation("", stdout, stderr, args)
	inv.Dir = WorkingDirOf(os.Getwd())
	return inv
}

// checkoutRoot is dir as a workspace.CheckoutRoot, its symlinks resolved as
// git resolves --show-toplevel (t.TempDir sits under /var -> /private/var on
// darwin).
func checkoutRoot(t testing.TB, dir string) workspace.CheckoutRoot {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	root, err := workspace.ParseCheckoutRoot(resolved)
	if err != nil {
		t.Fatalf("ParseCheckoutRoot(%q): %v", resolved, err)
	}
	return root
}

// testProject is the project config layer Resolve gives the checkout at root.
func testProject(root workspace.CheckoutRoot) config.ProjectLayer {
	return config.CheckoutProject(testEnv(), pathspec.New(root.String()))
}

// repositoryOf is the repository a standard checkout at dir has, <dir>/.git,
// named without asking git: a fixture whose dir is no repository gets one
// whose questions fail, as asking that dir's checkout did.
func repositoryOf(t testing.TB, dir string) workspace.Repository {
	t.Helper()
	return workspace.LocationFromStorageDir(filepath.Join(checkoutRoot(t, dir).String(), ".git", "links")).Repository()
}

// A run's stderr is written from more than one goroutine — a routine store
// hold reports a failed heartbeat from its own — so the writer Run hands its
// callees lets one Write through at a time, whatever the caller passed in.
func TestSerialWriterLetsOneWriteThroughAtATime(t *testing.T) {
	t.Parallel()
	var under overlapWriter
	w := &serialWriter{w: &under}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				if _, err := w.Write([]byte("x")); err != nil {
					t.Errorf("Write() error = %v", err)
				}
			}
		})
	}
	wg.Wait()
	if under.overlapped.Load() {
		t.Fatal("two writes reached the caller's writer at once")
	}
}

// A file Run is handed stays that file, so a child it starts on it (the $EDITOR
// `lit workflows edit` opens) gets the descriptor rather than a pipe; any
// other writer is serialized.
func TestSerializeWritesKeepsAFileAFile(t *testing.T) {
	t.Parallel()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if got := serializeWrites(file); got != io.Writer(file) {
		t.Fatalf("serializeWrites(file) = %T, want the file itself", got)
	}
	var buf strings.Builder
	if _, ok := serializeWrites(&buf).(*serialWriter); !ok {
		t.Fatalf("serializeWrites(&strings.Builder{}) = %T, want *serialWriter", serializeWrites(&buf))
	}
}

// overlapWriter records whether a Write began while another was in flight.
type overlapWriter struct {
	inFlight   atomic.Int32
	overlapped atomic.Bool
}

func (o *overlapWriter) Write(p []byte) (int, error) {
	if o.inFlight.Add(1) > 1 {
		o.overlapped.Store(true)
	}
	runtime.Gosched()
	o.inFlight.Add(-1)
	return len(p), nil
}
