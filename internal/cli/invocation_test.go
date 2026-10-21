package cli

import (
	"io"
	"os"
	"strings"
)

// dirInvocation is the invocation a test runs lit with in dir. stdin is empty,
// as /dev/null is to a `go test` process.
func dirInvocation(dir string, stdout, stderr io.Writer, args []string) Invocation {
	return Invocation{Dir: WorkingDirOf(dir, nil), Args: args, Stdin: strings.NewReader(""), Stdout: stdout, Stderr: stderr}
}

// cwdInvocation is dirInvocation for a test that steers Run by changing the
// process's directory; links-testperf-xxsx.2.1 hands those tests their
// directory instead.
func cwdInvocation(stdout, stderr io.Writer, args []string) Invocation {
	inv := dirInvocation("", stdout, stderr, args)
	inv.Dir = WorkingDirOf(os.Getwd())
	return inv
}
