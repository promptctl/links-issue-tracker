// Package testoracle locates the external CLIs the test suite uses as
// oracles — independent checkers such as `dolt` and `claude` whose verdict a
// test trusts over its own reading of the code — for the tests that skip when
// their oracle is missing. Neither CLI ships with lit, so a developer without
// one gets the skip. CI installs every oracle in every workflow that runs
// tests (`.github/actions/install-<name>`), so there a missing oracle is a
// broken workflow and fails the test: a skip would turn off the gate and
// leave the run green. [LAW:no-silent-failure] [LAW:single-enforcer] Every
// oracle skip is decided here; a test that runs `dolt` through
// internal/doltcli fails without it everywhere and needs no decision.
package testoracle

import (
	"os"
	"os/exec"
	"testing"
)

// Path returns the absolute path of the oracle CLI name, skipping t when the
// CLI is not on PATH and failing t instead when running under GitHub Actions.
func Path(t testing.TB, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	switch {
	case err == nil:
		return path
	case os.Getenv("GITHUB_ACTIONS") == "true":
		t.Fatalf("%s not on PATH, but CI installs every test oracle (.github/actions/install-%s): %v", name, name, err)
	default:
		t.Skipf("%s not on PATH; skipping this oracle check", name)
	}
	return ""
}
