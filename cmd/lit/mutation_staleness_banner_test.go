//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPushFailureBannerReachesMutationOnlySession is the acceptance pin for
// links-sync-pgct.10's visibility contract: with on-change cadence (the
// shipped default) and a remote that has become unreachable, a session that
// runs ONLY mutating commands sees a loud push-failure signal within a
// bounded time window — it does not have to happen to run backlog/next/show,
// the three read commands the staleness banner was originally wired into.
// links-sync-pgct.3's delivery test covers the happy half (a mutation's data
// reaches the remote); this is the failure half (a mutation's data NOT
// reaching the remote reaches the operator).
//
// The complementary no-cry-wolf property is pinned first: while the remote is
// healthy, a mutating command must NOT warn, even though every mutating
// command is momentarily "ahead" at its own end (its commit is pushed by a
// mirror that only runs after the process exits) — which is exactly why the
// banner's predicate is "the last push attempt FAILED", not the ahead count.
//
// The poll re-runs a real mutating command until its own output carries the
// banner, because the failing push is a detached background mirror with no
// completion signal available here; a fixed sleep would encode a timing bet.
// [LAW:no-ambient-temporal-coupling] If the marker/banner chain never fires,
// the poll exhausts its deadline and fails — a real regression signal.
func TestPushFailureBannerReachesMutationOnlySession(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	// Isolate config so the run exercises the shipped default cadence, exactly
	// as the eager-push delivery test does.
	xdgConfigHome := filepath.Join(base, "xdg-config")
	if err := os.Mkdir(xdgConfigHome, 0o755); err != nil {
		t.Fatalf("mkdir xdg config home: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)

	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "banner@test.co")
	runGit(t, root, "config", "user.name", "banner")
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-m", "seed")
	branch := gitCurrentBranch(t, root)

	remoteGit := filepath.Join(base, "remote.git")
	runGit(t, base, "init", "--bare", "remote.git")
	runGit(t, root, "remote", "add", "origin", remoteGit)
	runGit(t, root, "push", "-u", "origin", branch)

	// Establish a healthy, connected sync relationship. The explicit bootstrap
	// push also records the push-outcome marker as "pushed", so the healthy
	// assertion below starts from a truthful baseline.
	if out, err := runLit(t, root, self, map[string]string{disableAutoSyncEnvVar: "1"},
		"init", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out)
	}
	if out, err := runLit(t, root, self, map[string]string{disableAutoSyncEnvVar: "1"},
		"sync", "push", "--set-upstream"); err != nil {
		t.Fatalf("bootstrap lit sync push: %v\noutput:\n%s", err, out)
	}

	// No cry-wolf: a mutation on a HEALTHY workspace prints no failure banner,
	// even though its own commit has not been pushed yet at print time.
	healthyOut, err := runLit(t, root, self, map[string]string{disableAutoSyncEnvVar: "0"},
		"new", "--title", "healthy-mutation", "--topic", "demo")
	if err != nil {
		t.Fatalf("lit new (healthy): %v\noutput:\n%s", err, healthyOut)
	}
	if strings.Contains(healthyOut, "FAILING") {
		t.Fatalf("healthy mutation warned about push failure:\n%s", healthyOut)
	}

	// The remote becomes unreachable — the field shape this epic's incident
	// took (a remote that stops answering while mutations keep landing
	// locally). Nothing else in this test touches a read command from here on.
	runGit(t, root, "remote", "set-url", "origin", filepath.Join(base, "gone", "nowhere.git"))

	const pollTimeout = 20 * time.Second
	const pollInterval = 300 * time.Millisecond
	deadline := time.Now().Add(pollTimeout)
	var lastOut string
	for time.Now().Before(deadline) {
		out, err := runLit(t, root, self, map[string]string{disableAutoSyncEnvVar: "0"},
			"new", "--title", "mutation-against-dead-remote", "--topic", "demo")
		if err != nil {
			t.Fatalf("lit new (mutation-only chain): %v\noutput:\n%s", err, out)
		}
		lastOut = out
		if strings.Contains(out, "FAILING") && strings.Contains(out, "lit sync push") {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("no push-failure banner reached a mutation-only session within %s of the remote dying; last mutation output:\n%s",
		pollTimeout, lastOut)
}
