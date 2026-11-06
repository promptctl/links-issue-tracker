package main

import (
	"context"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/interrupt"
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
	// A failure claims the exit before it is printed, so the code on stderr is
	// the code the process exits with. One the hard exit already decided is
	// not printed: the process is ending on the interrupt, and the invocation's
	// event records what the command ended with.
	fail := func(err error) {
		decided := interrupt.Claim(ctx, cli.CommandExitCode(err))
		if !decided.Hard() {
			cli.WriteCommandError(os.Stderr, err)
		}
		exit(decided.Code)
	}
	// [LAW:effects-at-boundaries] The one place lit reads its process: the run
	// below is handed everything it would otherwise read from it.
	// The released-schema ceiling and the raise policy are read from this
	// binary's build stamp here and nowhere below: they ride the run's Env to
	// every store open.
	released, err := version.ReadReleasedSchemaCeiling()
	if err != nil {
		fail(err)
	}
	inv := cli.Invocation{
		Dir:    cli.WorkingDirOf(os.Getwd()),
		Args:   os.Args[1:],
		Env:    environ.Of(os.Environ()).WithReleasedSchema(released).WithRaisePolicy(version.RaisePolicyOf(version.Origin)),
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	if err := cli.Run(ctx, inv); err != nil {
		fail(err)
	}
	exit(cli.ExitOK)
}
