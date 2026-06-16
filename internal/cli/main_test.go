package cli

import (
	"os"
	"testing"
)

// TestMain disables automatic sync for the whole cli test package. Many cli
// tests drive the real CLI in-process; without this, a command's post-run hook
// would spawn the on-change push mirror (via os.Executable(), which under
// `go test` is the test binary) and run an inline receive (a real network fetch)
// as a side effect of unrelated tests. The receive path is exercised explicitly
// by TestAutomaticReceiveFastForwardsEstablishedClone, which clears this switch
// for its own workspace, so disabling it package-wide loses no coverage.
func TestMain(m *testing.M) {
	if err := os.Setenv(disableAutoSyncEnvVar, "1"); err != nil {
		panic("set " + disableAutoSyncEnvVar + ": " + err.Error())
	}
	os.Exit(m.Run())
}
