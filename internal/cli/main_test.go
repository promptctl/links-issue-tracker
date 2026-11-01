package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dirtemplate"
	"github.com/promptctl/links-issue-tracker/internal/store"
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

// suiteStallDeadline is the deadline this suite runs the mirror's hold and the
// automatic receive under, in this process and in the detached workers it
// spawns. Both shipped figures sit close enough to the local work these tests
// really do that a busy machine reaches them: the hold bounds a clone, and a
// receive that follows a re-root measures about a third of its deadline. So a
// test with no interest in either would take its verdict from the load.
// [LAW:no-ambient-temporal-coupling] The push deadline stays as shipped: a whole
// mirror cycle against these tests' file remotes measures about a twelfth of it.
// A minute is still a stall detector: over ten times that work, and far below
// go test's timeout, so a hang ends inside the test that met it and the
// package's other verdicts stand. A test about either deadline installs its own
// figure.
const suiteStallDeadline = time.Minute

// TestMain disables automatic sync for the whole cli test package. Many cli
// tests drive the real CLI in-process; without this, a command's post-run hook
// would spawn the on-change push mirror (via os.Executable(), which under
// `go test` is the test binary) and the receive worker (a real network fetch)
// as a side effect of unrelated tests. The receive path is exercised explicitly,
// by tests that run the receive worker's body in-process (receiveNow), so
// disabling it package-wide loses no coverage.
func TestMain(m *testing.M) {
	store.MirrorHoldBudget, store.ReceiveDeadline = suiteStallDeadline, suiteStallDeadline
	// A detached worker spawns os.Executable(), which here is this test binary.
	// Run as that worker rather than as a second copy of the whole suite; the
	// suite's tests call the receive body directly and never mean to spawn one.
	if len(os.Args) > 1 && (os.Args[1] == backgroundReceiveSubcommand || os.Args[1] == backgroundMirrorSubcommand) {
		if err := Run(context.Background(), cwdInvocation(os.Stdout, os.Stderr, os.Args[1:])); err != nil {
			os.Exit(WriteCommandError(os.Stderr, err))
		}
		os.Exit(0)
	}
	if err := os.Setenv(DisableAutoSyncEnvVar, "1"); err != nil {
		panic("set " + DisableAutoSyncEnvVar + ": " + err.Error())
	}
	stateHome := isolateXDGHome("XDG_STATE_HOME", "lit-cli-state-")
	configHome := isolateXDGHome("XDG_CONFIG_HOME", "lit-cli-config-")
	code := dirtemplate.Run(m, storeTemplate)
	_ = os.RemoveAll(stateHome)
	_ = os.RemoveAll(configHome)
	os.Exit(code)
}

// isolateXDGHome points the XDG base directory key names at a fresh directory
// for the rest of the process, and the detached workers a test spawns inherit
// it. Every Run appends its invocation event under XDG_STATE_HOME: what a test
// run writes belongs in a directory the suite owns, never in the developer's
// own events file. Every workspace command reads the global config layer under
// XDG_CONFIG_HOME, so the developer's own settings stay out of the suite too.
func isolateXDGHome(key, prefix string) string {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		panic("create isolated " + key + ": " + err.Error())
	}
	if err := os.Setenv(key, dir); err != nil {
		panic("set " + key + ": " + err.Error())
	}
	return dir
}
