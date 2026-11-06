package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dirtemplate"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// Parallelism convention for this package: a test calls t.Parallel() unless it
// swaps a package variable (a store deadline, time.Local, a seam function),
// which every parallel reader of it would race. A test hands lit its working
// directory and environment through the Invocation
// (dirInvocation, setRunEnv) instead of the process's, so two runs overlapping
// never see each other's workspace; TestNoTestChangesTheProcess keeps any test
// from changing the process's.
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

// testMirrorPushDeadlineEnvVar carries a test's push deadline to a mirror it
// runs as a child of this binary, where store.MirrorPushDeadline is the
// child's own and no other test reads it.
const testMirrorPushDeadlineEnvVar = "LIT_TEST_MIRROR_PUSH_DEADLINE"

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
		if deadline, ok := os.LookupEnv(testMirrorPushDeadlineEnvVar); ok {
			d, err := time.ParseDuration(deadline)
			if err != nil {
				panic(testMirrorPushDeadlineEnvVar + ": " + err.Error())
			}
			store.MirrorPushDeadline = d
		}
		if err := Run(context.Background(), Invocation{Dir: WorkingDirOf(os.Getwd()), Args: os.Args[1:], Env: testEnv(), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}); err != nil {
			os.Exit(WriteCommandError(os.Stderr, err))
		}
		os.Exit(0)
	}
	if err := os.Setenv(DisableAutoSyncEnvVar, "1"); err != nil {
		panic("set " + DisableAutoSyncEnvVar + ": " + err.Error())
	}
	// Every Run appends its invocation event under XDG_STATE_HOME and reads the
	// global config layer under XDG_CONFIG_HOME, and Dolt reads its own home
	// in-process on every store open, from the process environment no run's
	// Env reaches: directories the suite owns keep the developer's events
	// file, settings and ~/.dolt out of every test, and the detached workers a
	// test spawns inherit them.
	var suiteDirs []string
	for _, home := range []struct{ key, prefix string }{
		{"XDG_STATE_HOME", "lit-cli-state-"},
		{"XDG_CONFIG_HOME", "lit-cli-config-"},
		{"DOLT_ROOT_PATH", "lit-cli-dolt-"},
	} {
		dir, err := os.MkdirTemp("", home.prefix)
		if err != nil {
			panic("create isolated " + home.key + ": " + err.Error())
		}
		if err := os.Setenv(home.key, dir); err != nil {
			panic("set " + home.key + ": " + err.Error())
		}
		suiteDirs = append(suiteDirs, dir)
	}
	// The git a test runs, and the git Dolt runs, read the suite's git config
	// in place of the machine's, so the developer's ~/.gitconfig (signing,
	// hooks) never reaches a test. HOME itself stays, because the go toolchain
	// a test runs finds its caches under it. The tests' repositories expect
	// their first branch to be master, which git's built-in default does not
	// promise.
	gitDir, err := os.MkdirTemp("", "lit-cli-git-")
	if err != nil {
		panic("create isolated git config: " + err.Error())
	}
	suiteDirs = append(suiteDirs, gitDir)
	gitConfig := filepath.Join(gitDir, "config")
	if err := os.WriteFile(gitConfig, []byte("[init]\n\tdefaultBranch = master\n"), 0o644); err != nil {
		panic("write isolated git config: " + err.Error())
	}
	for key, value := range map[string]string{"GIT_CONFIG_GLOBAL": gitConfig, "GIT_CONFIG_NOSYSTEM": "1"} {
		if err := os.Setenv(key, value); err != nil {
			panic("set " + key + ": " + err.Error())
		}
	}
	code := dirtemplate.Run(m, storeTemplate)
	for _, dir := range suiteDirs {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}
