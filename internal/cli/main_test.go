package cli

import (
	"context"
	"os"
	"testing"
)

// Parallelism convention for this package: a test calls t.Parallel() exactly
// when it is transitively free of process-global mutation — no os.Chdir (the
// runCLIInDir/chdirTempRepo helpers), no t.Setenv, no os.Stdout swap. The
// in-process e2e tests fail that test by construction: they point the one
// process-wide cwd and environment at a per-test repo before calling Run, so
// two of them overlapping would silently run against each other's workspace.
// They stay serial until Run accepts its working directory and environment as
// parameters instead of reading process globals.
// [LAW:no-shared-mutable-globals] the process cwd/env is exactly such a global.

// TestMain disables automatic sync for the whole cli test package. Many cli
// tests drive the real CLI in-process; without this, a command's post-run hook
// would spawn the on-change push mirror (via os.Executable(), which under
// `go test` is the test binary) and the receive worker (a real network fetch)
// as a side effect of unrelated tests. The receive path is exercised explicitly,
// by tests that run the receive worker's body in-process (receiveNow), so
// disabling it package-wide loses no coverage.
func TestMain(m *testing.M) {
	// A detached worker spawns os.Executable(), which here is this test binary.
	// Run as that worker rather than as a second copy of the whole suite; the
	// suite's tests call the receive body directly and never mean to spawn one.
	if len(os.Args) > 2 && os.Args[1] == "sync" &&
		(os.Args[2] == backgroundReceiveSubcommand || os.Args[2] == backgroundMirrorSubcommand) {
		if err := Run(context.Background(), os.Stdout, os.Stderr, os.Args[1:]); err != nil {
			os.Exit(WriteCommandError(os.Stderr, err))
		}
		os.Exit(0)
	}
	if err := os.Setenv(DisableAutoSyncEnvVar, "1"); err != nil {
		panic("set " + DisableAutoSyncEnvVar + ": " + err.Error())
	}
	os.Exit(m.Run())
}
