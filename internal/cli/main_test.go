package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/dirtemplate"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// Parallelism convention for this package: a test calls t.Parallel() unless it
// swaps a package variable (time.Local, a seam function),
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
// package's other verdicts stand. A test about either deadline hands its runs
// its own figure.
const suiteStallDeadline = time.Minute

// suiteDeadlines is the deadlines every run this suite starts is handed, and
// so every worker those runs spawn (workerHandoff): the shipped ones, with the
// hold and the receive at the suite's stall deadline.
func suiteDeadlines() store.Deadlines {
	d := store.ShippedDeadlines()
	d.MirrorHold, d.Receive = suiteStallDeadline, suiteStallDeadline
	return d
}

// suiteLitProgram is the name go build gives the program of ./cmd/lit, which
// os/exec finds again with the suffix Windows wants.
const suiteLitProgram = "lit"

// suiteLitBuildDeadline bounds the build of the suite's lit: a cold build of
// cmd/lit with the race detector, on a loaded machine, is minutes.
const suiteLitBuildDeadline = 20 * time.Minute

// suiteLit holds lit itself, cmd/lit, built once per test binary before any
// test runs and stamped as a release of this tree is (the schema it carries,
// raising as a release does), so its runs read what suiteenv.Of hands this
// binary's. It is the program a test that starts a real detached worker
// starts (useRealWorkers): the worker is lit as it ships, a process of its
// own, and no test pays for the build. [LAW:one-source-of-truth]
var suiteLit = dirtemplate.New("bin", func(dir string) error {
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	versionPkg := reflect.TypeOf(version.SchemaCeiling{}).PkgPath()
	ldflags := "-X " + versionPkg + ".ReleasedSchema=" + version.ReleasedSchemaCarried + " -X " + versionPkg + ".Origin=" + version.OriginRelease
	args := []string{"build", "-buildvcs=false", "-ldflags", ldflags}
	if raceEnabled {
		args = append(args, "-race")
	}
	// An -o ending in a separator is a directory go build makes and names the
	// program in.
	args = append(args, "-o", dir+string(filepath.Separator), "./cmd/lit")
	ctx, cancel := context.WithTimeout(context.Background(), suiteLitBuildDeadline)
	defer cancel()
	// The program is named at the spawn, so what the build starts is go, in
	// this process's own environment, and nothing a caller could hand it.
	// [LAW:types-are-the-program]
	build := exec.CommandContext(ctx, "go", args...)
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go %v in %s: %w\n%s", args, root, err, out)
	}
	return nil
})

// moduleRoot is the directory of the go.mod above this file, as it was
// compiled.
func moduleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return "", fmt.Errorf("this test binary does not know where its source is (%q), so it cannot build cmd/lit", file)
	}
	for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("look for go.mod in %s: %w", dir, err)
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("no go.mod above %s", file)
		}
	}
}

// suiteLitPath is suiteLit's program. TestMain built it before any test ran.
func suiteLitPath() (string, error) {
	dir, err := suiteLit.Dir()
	if err != nil {
		return "", fmt.Errorf("build the suite's lit: %w", err)
	}
	return filepath.Join(dir, suiteLitProgram), nil
}

// TestMain disables automatic sync for the whole cli test package. Many cli
// tests drive the real CLI in-process; without this, a command's post-run hook
// would spawn the on-change push mirror and the receive worker (a real network
// fetch) as a side effect of unrelated tests. The receive path is exercised
// explicitly, by tests that run the receive worker's body in-process
// (receiveNow), so disabling it package-wide loses no coverage. A test that
// starts a worker anyway, autosync off or on, is refused by the launcher its
// runs are handed (runWorkers).
func TestMain(m *testing.M) {
	// Built first, before any test runs, so no test's time is the build's,
	// and before the suite makes anything a failure here would leave behind:
	// a failed build removes its own base.
	if _, err := suiteLitPath(); err != nil {
		panic(err.Error())
	}
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
	code := dirtemplate.Run(m, storeTemplate, suiteLit)
	for _, dir := range suiteDirs {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}
