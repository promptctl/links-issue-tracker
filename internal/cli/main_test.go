package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/dirtemplate"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// Parallelism convention for this package: a test calls t.Parallel() unless it
// swaps a package variable (time.Local, a seam function),
// which every parallel reader of it would race. A test hands lit its working
// directory and environment through the Invocation
// (dirInvocation, setRunEnv) instead of the process's, so two runs overlapping
// never see each other's workspace; TestNoTestChangesTheProcess keeps any test
// from changing the process's.
// [LAW:no-shared-mutable-globals] the process cwd/env is exactly such a global.

// TestMain disables automatic sync for the whole cli test package. Many cli
// tests drive the real CLI in-process; without this, a command's post-run hook
// would spawn the on-change push mirror and the receive worker (a real network
// fetch) as a side effect of unrelated tests. The receive path is exercised
// explicitly, by tests that run the receive worker's body in-process
// (receiveNow), so disabling it package-wide loses no coverage. A test that
// starts a worker anyway, autosync off or on, is refused by the launcher its
// runs are handed (refusedWorkers).
func TestMain(m *testing.M) {
	if err := os.Setenv(DisableAutoSyncEnvVar, "1"); err != nil {
		panic("set " + DisableAutoSyncEnvVar + ": " + err.Error())
	}
	// Children inherit this environment, so none reads a config file the
	// developer's shell points at.
	for _, key := range config.PathEnvVars() {
		if err := os.Unsetenv(key); err != nil {
			panic("unset " + key + ": " + err.Error())
		}
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
		{store.MirrorRootEnvVar, "lit-cli-mirrors-"},
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
	if err := removeRealLit(); err != nil {
		fmt.Fprintf(os.Stderr, "remove the real lit: %v\n", err)
		code = max(code, 1)
	}
	for _, dir := range suiteDirs {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}
