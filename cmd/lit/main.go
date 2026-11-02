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
	// the process never becomes SIGKILL-only.
	ctx, stop := interrupt.Guard(context.Background(), interrupt.DefaultGrace)
	defer stop()
	// [LAW:effects-at-boundaries] The one place lit reads its process: the run
	// below is handed everything it would otherwise read from it.
	// The released-schema ceiling and the raise policy are read from this
	// binary's build stamp here and nowhere below: they ride the run's Env to
	// every store open.
	released, err := version.ReadReleasedSchemaCeiling()
	if err != nil {
		os.Exit(cli.WriteCommandError(os.Stderr, err))
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
		os.Exit(cli.WriteCommandError(os.Stderr, err))
	}
}
