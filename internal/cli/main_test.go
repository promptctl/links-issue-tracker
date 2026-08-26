package cli

import (
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
// `go test` is the test binary) and run an inline receive (a real network fetch)
// as a side effect of unrelated tests. The receive path is exercised explicitly
// by TestAutomaticReceiveFastForwardsEstablishedClone, which clears this switch
// for its own workspace, so disabling it package-wide loses no coverage.
func TestMain(m *testing.M) {
	if err := os.Setenv(DisableAutoSyncEnvVar, "1"); err != nil {
		panic("set " + DisableAutoSyncEnvVar + ": " + err.Error())
	}
	os.Exit(m.Run())
}
