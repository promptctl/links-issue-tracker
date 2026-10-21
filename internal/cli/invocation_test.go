package cli

import (
	"io"
	"os"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/environ"
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

// cwdInvocation is dirInvocation for a test that steers Run by changing the
// process's directory; links-testperf-xxsx.2.1 hands those tests their
// directory instead.
func cwdInvocation(stdout, stderr io.Writer, args []string) Invocation {
	inv := dirInvocation("", stdout, stderr, args)
	inv.Dir = WorkingDirOf(os.Getwd())
	return inv
}
