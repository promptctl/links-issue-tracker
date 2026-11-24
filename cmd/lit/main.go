package main

import (
	"context"
	"fmt"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/deadline"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/interrupt"
	"github.com/promptctl/links-issue-tracker/internal/runfacts"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

func main() {
	// [LAW:no-ambient-temporal-coupling] The process's interrupt-shutdown
	// lifecycle has one owner: a SIGINT/SIGTERM cancels this context (so the
	// post-write auto-sync abandons cleanly and releases the store's commit lock)
	// and, if the in-flight work ignores the cancel, escalates to a hard exit so
	// the process never becomes SIGKILL-only. It is also the one way the
	// process exits: exit ends it with the code the guard decided, which is the
	// code the invocation's event records.
	ctx, exit := interrupt.Guard(context.Background(), interrupt.DefaultGrace)
	// A failure claims the exit before it is printed, so the code
	// WriteCommandError returns is the code the process exits with. A failure after the hard exit decided
	// is printed under the interrupt and code the process is ending on.
	fail := func(err error) {
		decided := interrupt.Claim(ctx, cli.CommandExitCode(err))
		if decided.Hard {
			fmt.Fprintf(os.Stderr, "lit: hard-exited with %d after %s; the command then ended: %v\n", decided.Code, decided.Signal, err)
		} else {
			cli.WriteCommandError(os.Stderr, err)
		}
		exit(decided.Code)
	}
	inv, err := processInvocation()
	if err != nil {
		fail(err)
	}
	if err := cli.Run(ctx, inv); err != nil {
		fail(err)
	}
	exit(cli.ExitOK)
}

// processInvocation is the run this process is: everything the run would
// otherwise read from the process, read here, once. [LAW:effects-at-boundaries]
// The run's facts are built here, once: the released-schema ceiling and the
// raise policy from this binary's build stamp, read nowhere below, and the
// shipped deadlines. They ride the run's Env whole to every store open, and
// the run hands them to each worker it spawns. The workers are started as this
// binary, so a process that cannot name its own executable fails here, before
// the run, rather than at its first spawn.
func processInvocation() (cli.Invocation, error) {
	released, err := version.ReadReleasedSchemaCeiling()
	if err != nil {
		return cli.Invocation{}, err
	}
	facts, err := runfacts.Of(deadline.Shipped(), released, version.RaisePolicyOf(version.Origin))
	if err != nil {
		return cli.Invocation{}, fmt.Errorf("compose the run's facts: %w", err)
	}
	program, err := os.Executable()
	if err != nil {
		return cli.Invocation{}, fmt.Errorf("resolve lit's own executable, which its background workers run as: %w", err)
	}
	return cli.Invocation{
		Dir:     cli.WorkingDirOf(os.Getwd()),
		Args:    os.Args[1:],
		Env:     environ.Of(os.Environ()).WithFacts(facts),
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Workers: cli.LaunchWorkersAs(program),
	}, nil
}
