package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/environ"
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
