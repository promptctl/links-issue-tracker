//go:build !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestEagerPushOnDefaultCadenceReachesRemoteWithoutExplicitPush is the
// acceptance pin for links-sync-pgct.3: on a connected workspace with no
// [sync] config at all — the shipped default — a mutating command's change
// reaches the remote without any explicit `lit sync push` step. This is the
// literal gap the field incident exposed: push staying a manual act nobody
// remembered to do stranded 25 local changes for days with nothing surfacing
// the drift. The on-change mirror and cadence machinery already existed
// (#226, #227) as an opt-in; this test pins that they now actually fire for a
// workspace that never touched `[sync] cadence`, by proving the contract
// (does the remote observe the change) rather than the mechanism.
//
// The oracle is an independent `dolt clone` of the bare git remote, fetched
// and inspected directly — not lit's own "sync: N local change(s) not
// pushed" banner (links-sync-pgct.2). That banner reads freshness through the
// same tracking-ref bookkeeping a known pre-existing quirk leaves briefly
// wrong immediately after a workspace's very first `--set-upstream` push
// (reproduced by hand while writing this test: the ahead-count can read
// stale for a beat even once the data has genuinely landed), so it is not a
// reliable ground truth for "did the bytes actually reach the remote". The
// dolt CLI is already this project's sanctioned test oracle (see
// .github/workflows/ci.yml, which installs it for exactly this reason) and
// never shares lit's embedded engine or its path cache, so counting commits
// on `remotes/origin/<branch>` in a freshly fetched, never-otherwise-touched
// clone is independent ground truth. [LAW:behavior-not-structure]
//
// It polls that independent oracle with a bounded timeout instead of a fixed
// sleep, since the mirror is a detached background process with no other
// completion signal available to a caller. [LAW:no-ambient-temporal-coupling]
// If eager push never fired, this single local mutation reaches the remote by
// no other means (nothing else pushes in this test), so the poll exhausts its
// deadline and fails — a real regression signal, not a vacuous pass. This was
// confirmed by hand: reverting the default to on-push left the oracle's
// commit count unchanged for the full poll window, while the on-change
// default delivers within about a second.
func TestEagerPushOnDefaultCadenceReachesRemoteWithoutExplicitPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not available")
	}
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	// This test's whole claim is about config.Load's shipped DEFAULT, so it
	// must not accidentally inherit a real `sync.cadence` from whatever global
	// config.toml happens to exist on the machine running the suite — litEnv
	// otherwise passes the real process environment straight through, host XDG
	// config included. Every child is pointed at an isolated, always-empty XDG
	// config dir instead, as an explicit per-call override (isolatedEnv) rather
	// than a t.Setenv mutation of this process — the env var is consumed only
	// by the children, and keeping it out of the test process's environment is
	// what lets this test run in parallel. [LAW:no-ambient-temporal-coupling]
	xdgConfigHome := filepath.Join(base, "xdg-config")
	if err := os.Mkdir(xdgConfigHome, 0o755); err != nil {
		t.Fatalf("mkdir xdg config home: %v", err)
	}

	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "eager@test.co")
	runGit(t, root, "config", "user.name", "eager")
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-m", "seed")
	branch := gitCurrentBranch(t, root)

	remoteGit := filepath.Join(base, "remote.git")
	runGit(t, base, "init", "--bare", "remote.git")
	runGit(t, root, "remote", "add", "origin", remoteGit)
	// A real `git push` first is required: `lit sync push` deliberately skips
	// ("remote has no refs yet") until the git remote itself carries real refs,
	// so it never orphans dolt data onto a remote nobody has actually connected
	// git to. That precondition is unrelated to cadence and true of both values.
	runGit(t, root, "push", "-u", "origin", branch)

	// Bootstrap: init, then one manual push to establish an already-connected
	// sync relationship — this test is about an ESTABLISHED connected
	// workspace's next mutation, not the never-synced bootstrap edge case.
	if out, err := runLit(t, root, self, isolatedEnv(xdgConfigHome, "1"),
		"init", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out)
	}
	if out, err := runLit(t, root, self, isolatedEnv(xdgConfigHome, "1"),
		"sync", "push", "--set-upstream"); err != nil {
		t.Fatalf("bootstrap lit sync push: %v\noutput:\n%s", err, out)
	}

	// Independent oracle: a plain `dolt clone` of the bare remote, touched by
	// nothing else in this test. Its baseline commit count is read once, before
	// the mutation, so the poll below detects "at least one new commit landed"
	// rather than depending on an exact expected count. Both the baseline and
	// every poll read the same explicit `remotes/origin/<branch>` ref — never
	// the ambiguous "current checkout" — so the comparison cannot be skewed by
	// dolt's or the bare repo's own notion of a "default" branch disagreeing
	// with the branch this test actually pushed.
	ref := "remotes/origin/" + branch
	oracleDir := filepath.Join(base, "oracle-clone")
	runDolt(t, base, "clone", "file://"+remoteGit, oracleDir)
	baseline := doltRefCommitCount(t, oracleDir, ref)

	// No config.toml exists anywhere under xdgConfigHome: this run exercises
	// config.Load's shipped default, not an explicit on-change opt-in.
	if out, err := runLit(t, root, self, isolatedEnv(xdgConfigHome, "0"),
		"new", "--title", "eager-push", "--topic", "demo"); err != nil {
		t.Fatalf("lit new: %v\noutput:\n%s", err, out)
	}

	const pollTimeout = 15 * time.Second
	const pollInterval = 300 * time.Millisecond
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		// Tolerant of a transient fetch error (e.g. hitting the remote mid-push
		// from the background mirror): treat it as "not yet" and retry within the
		// poll window rather than failing the test on a one-off race unrelated to
		// the thing under test. [LAW:no-silent-failure] exception: the fetch
		// output is discarded here only because a genuine, persistent failure
		// still surfaces — as a poll-timeout failure below, not silently.
		if _, err := tryDolt(oracleDir, "fetch", "origin"); err == nil {
			if count := doltRefCommitCount(t, oracleDir, ref); count > baseline {
				return
			}
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("mutation never reached the remote without an explicit push, %s after lit new returned (default cadence should be on-change)", pollTimeout)
}

// isolatedEnv builds the child-env overrides for a test whose claim is about
// config.Load's shipped defaults: an isolated (empty) XDG config dir so no
// host config.toml leaks in, plus the auto-sync switch. Passing these
// per-call, instead of t.Setenv on the shared process environment, is what
// permits t.Parallel across this package's tests — every env var here is
// consumed only by the child lit processes, never by the test process itself.
func isolatedEnv(xdgConfigHome, disableAutoSync string) map[string]string {
	return map[string]string{
		"XDG_CONFIG_HOME":     xdgConfigHome,
		disableAutoSyncEnvVar: disableAutoSync,
	}
}

// gitCurrentBranch reads the checked-out branch name rather than assuming
// "master"/"main": the default varies by git version and machine config, and
// lit's own push resolves the sync branch from the same git state, so the
// test must agree with whatever that resolution actually picks.
func gitCurrentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "symbolic-ref", "--short", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git symbolic-ref --short HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// runDolt runs a dolt CLI subcommand to completion, failing the test on a
// non-zero exit. dolt is this project's sanctioned test oracle (installed by
// .github/workflows/ci.yml for that purpose), independent of lit's own
// embedded engine and its path cache. Use tryDolt instead where a transient
// failure (e.g. polling a remote mid-push) must not fail the test outright.
func runDolt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := tryDolt(dir, args...)
	if err != nil {
		t.Fatalf("dolt %s: %v\noutput:\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// tryDolt runs a dolt CLI subcommand and returns its combined output and any
// error, without failing the test — the non-fatal counterpart to runDolt.
func tryDolt(dir string, args ...string) (string, error) {
	cmd := exec.Command("dolt", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// doltRefCommitCount counts commits reachable from ref inside the dolt
// database at dir, via a plain SQL count — ground truth independent of lit's
// own freshness/ahead-count bookkeeping.
func doltRefCommitCount(t *testing.T, dir, ref string) int {
	t.Helper()
	query := "select count(*) as n from dolt_log(" + strconv.Quote(ref) + ")"
	out := runDolt(t, dir, "sql", "-q", query, "-r", "csv")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	count, err := strconv.Atoi(last)
	if err != nil {
		t.Fatalf("parse dolt_log count from %q: %v", out, err)
	}
	return count
}
