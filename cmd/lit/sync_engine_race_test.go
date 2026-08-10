//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBurstOfMutationsNeverHitsEngineReadOnlyCollision is the acceptance pin
// for links-sync-pgct.11: on a connected workspace with on-change cadence (the
// shipped default), a burst of several mutating lit commands run back-to-back
// must never surface Dolt's raw "database is read only" (or the online-GC
// reconnect variant) error, and every command must exit 0.
//
// The race this reproduces: each mutating command's on-change mirror is a
// detached subprocess that keeps running (spawning its own read-write
// embedded Dolt engine, pushing) after the command itself has returned.
// waitForParentExit only knows about its OWN spawning command's PID — it has
// no awareness that a DIFFERENT, still-running mirror (or the next
// command's own engine) might already hold the path's one read-write engine.
// Before links-sync-pgct.11's engine-write lock, this was a genuine,
// field-measured collision (see the embedded-dolt-one-readwrite-engine-per-path
// project memory: 6 of 15 rapid foreground commands failed this way against a
// concurrent background writer). This test drives the same shape — several
// `lit new` calls fired in immediate succession — end to end through the real
// CLI binary rather than the store package directly (see
// TestEngineWriteLockSerializesConcurrentOpen and
// TestEngineWriteLockSerializesOpenAgainstOpenSync in
// internal/store/engine_lock_test.go for the deterministic, non-timing-
// dependent proof of the underlying mechanism this test complements).
//
// Timing note: because each mirror is a real subprocess (fork/exec, its own
// engine open, a real git push), a burst run back-to-back on a local machine
// reliably produces genuine overlap between an earlier command's mirror and a
// later command's own engine open — the same overlap the project memory's "15
// rapid commands" reproduction observed. This test does not force the overlap
// deterministically (unlike the store-package tests above); it is corroborating
// end-to-end evidence, not the sole proof of the fix.
func TestBurstOfMutationsNeverHitsEngineReadOnlyCollision(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	// Isolate this run's config from whatever XDG config exists on the host,
	// same as TestEagerPushOnDefaultCadenceReachesRemoteWithoutExplicitPush:
	// this test's claim is about the shipped default cadence, not whatever a
	// developer's machine happens to have configured. [LAW:no-ambient-temporal-coupling]
	xdgConfigHome := filepath.Join(base, "xdg-config")
	if err := os.Mkdir(xdgConfigHome, 0o755); err != nil {
		t.Fatalf("mkdir xdg config home: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)

	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "burst@test.co")
	runGit(t, root, "config", "user.name", "burst")
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

	if out, err := runLit(t, root, self, map[string]string{disableAutoSyncEnvVar: "1"},
		"init", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out)
	}
	if out, err := runLit(t, root, self, map[string]string{disableAutoSyncEnvVar: "1"},
		"sync", "push", "--set-upstream"); err != nil {
		t.Fatalf("bootstrap lit sync push: %v\noutput:\n%s", err, out)
	}

	// No config.toml: this run exercises the shipped default (on-change), the
	// same way a fresh agent workspace does — and, unlike the bootstrap calls
	// above, does NOT set disableAutoSyncEnvVar, so every mutation below
	// spawns a real on-change mirror exactly as production does.
	const burstSize = 15
	for i := 0; i < burstSize; i++ {
		out, err := runLit(t, root, self, map[string]string{disableAutoSyncEnvVar: "0"},
			"new", "--title", "burst-issue", "--topic", "demo")
		if err != nil {
			t.Fatalf("lit new #%d failed: %v\noutput:\n%s", i, err, out)
		}
		lower := strings.ToLower(out)
		if strings.Contains(lower, "database is read only") || strings.Contains(lower, "cannot update manifest") {
			t.Fatalf("lit new #%d hit an engine open-collision instead of waiting for it to clear:\noutput:\n%s", i, out)
		}
		if strings.Contains(lower, "online garbage collection") && strings.Contains(lower, "reconnect") {
			t.Fatalf("lit new #%d hit an online-GC reconnect collision:\noutput:\n%s", i, out)
		}
	}
}
