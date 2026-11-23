// Command suitelit is lit as internal/cli's tests run it in the detached
// workers they spawn: the CLI over the suite's Env (suiteenv), under the
// suite's stall deadline, and under the push deadline a test asks a mirror
// for. The tests build it once per test binary and hand it to their runs as
// the program a worker runs (environ.Env.WithExecutable); nothing ships it.
//
// Both deadlines are process-level in a worker (store's package variables), so
// a test reaches them only through the process it spawns: the suite's figure
// is stamped into this binary when the tests build it, and a test's push
// deadline arrives in the variable the build names.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/suiteenv"
)

// Stamped by the build (-ldflags -X), from internal/cli's tests, so the figure
// and the variable's name each have one home: the suite that builds this.
// [LAW:one-source-of-truth]
var (
	// stallDeadline is the suite's stall deadline, which the mirror's hold
	// and the automatic receive run under in every process of the suite.
	stallDeadline string
	// pushDeadlineEnvVar names the variable a test sets a mirror's push
	// deadline in; unset, the push runs under the shipped one.
	pushDeadlineEnvVar string
)

func main() {
	stall, err := time.ParseDuration(stallDeadline)
	if err != nil {
		fail(fmt.Errorf("suitelit was built without a stall deadline (-X main.stallDeadline): %w", err))
	}
	if pushDeadlineEnvVar == "" {
		fail(fmt.Errorf("suitelit was built without the push deadline's variable (-X main.pushDeadlineEnvVar)"))
	}
	store.MirrorHoldBudget, store.ReceiveDeadline = stall, stall
	if deadline, ok := os.LookupEnv(pushDeadlineEnvVar); ok {
		d, err := time.ParseDuration(deadline)
		if err != nil {
			fail(fmt.Errorf("%s: %w", pushDeadlineEnvVar, err))
		}
		store.MirrorPushDeadline = d
	}
	inv := cli.Invocation{
		Dir:    cli.WorkingDirOf(os.Getwd()),
		Args:   os.Args[1:],
		Env:    suiteenv.Of(os.Environ()).WithExecutable(os.Executable),
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	if err := cli.Run(context.Background(), inv); err != nil {
		os.Exit(cli.WriteCommandError(os.Stderr, err))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "suitelit:", err)
	os.Exit(2)
}
