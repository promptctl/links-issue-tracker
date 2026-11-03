package cli

import (
	"context"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/version"
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

// dirInvocation is the invocation t runs lit with in dir. stdin is empty, as
// /dev/null is to a `go test` process, and its environment is t's (runEnv).
func dirInvocation(t testing.TB, dir string, stdout, stderr io.Writer, args []string) Invocation {
	return Invocation{Dir: WorkingDirOf(dir, nil), Args: args, Env: runEnv(t), Stdin: strings.NewReader(""), Stdout: stdout, Stderr: stderr}
}

// runEnvs holds each test's own environment entries, keyed by test name, so a
// test sets what its runs see without touching the process environment that
// every parallel test shares.
var runEnvs sync.Map // test name -> []string of KEY=value

// setRunEnv gives every run t and its subtests start the entry key=value,
// where t.Setenv would have set it for the whole process. It is undone when t
// ends.
func setRunEnv(t testing.TB, key, value string) {
	t.Helper()
	name := t.Name()
	prev, _ := runEnvs.Load(name)
	entries, _ := prev.([]string)
	runEnvs.Store(name, append(append([]string(nil), entries...), key+"="+value))
	if prev == nil {
		t.Cleanup(func() { runEnvs.Delete(name) })
	}
}

// runEnv is testEnv with the entries t and each test enclosing it set, the
// innermost setting of a key winning.
func runEnv(t testing.TB) environ.Env {
	var set []string
	parts := strings.Split(t.Name(), "/")
	for i := range parts {
		if entries, ok := runEnvs.Load(strings.Join(parts[:i+1], "/")); ok {
			set = append(set, entries.([]string)...)
		}
	}
	return testEnv().With(set...)
}

// testEnv is the test process's environment. Its released-schema ceiling is
// this tree's own highest migration, so the suite exercises every migration
// the tree carries. A test that sets entries of its own reads runEnv instead.
func testEnv() environ.Env {
	max, err := migrations.MaxVersion()
	if err != nil {
		panic("read the migration registry: " + err.Error())
	}
	return environ.Of(os.Environ()).WithReleasedSchema(version.SchemaCeiling{Max: max, Source: "the test suite runs every migration this tree carries"}).WithRaisePolicy(version.RaisePolicy{Release: true})
}

// A literal that leaves Env out holds the zero Env. Run refuses it, where the
// same invocation given an environment runs.
func TestRunRefusesAnInvocationGivenNoEnvironment(t *testing.T) {
	t.Parallel()
	inv := dirInvocation(t, t.TempDir(), io.Discard, io.Discard, []string{"version"})
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
		inv := dirInvocation(t, t.TempDir(), io.Discard, io.Discard, []string{"version"})
		drop(&inv)
		if err := Run(context.Background(), inv); err == nil || err.Error() != "the invocation was given no "+name {
			t.Errorf("lit version with no %s = %v, want it refused as given none", name, err)
		}
	}
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
func testProject(t testing.TB, root workspace.CheckoutRoot) config.ProjectLayer {
	return config.CheckoutProject(runEnv(t), pathspec.New(root.String()))
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
