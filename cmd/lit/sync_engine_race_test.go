//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBurstOfMutationsNeverHitsEngineReadOnlyCollision is the acceptance pin
// for links-sync-pgct.11: on a connected workspace with on-change cadence (the
// shipped default), a burst of several mutating lit commands run back-to-back
// must never surface Dolt's raw "database is read only" (or the online-GC
// reconnect variant) error, and every command must exit 0. Since
// links-sync-pgct.12 it is also the acceptance pin for the burst tail: every
// commit — including the final mutation's — reaches the remote with NO
// explicit sweep push, proved by the independent oracle poll at the end.
//
// The race this reproduces: each mutating command's on-change mirror is a
// detached subprocess that keeps running (spawning its own read-write
// embedded Dolt engine, pushing) after the command itself has returned.
// waitForParentExit only knows about its OWN spawning command's PID — it has
// no awareness that a DIFFERENT, still-running mirror (or the next
// command's own engine) might already hold the path's one read-write engine.
// Before links-sync-pgct.11's write-open serialization, this was a genuine,
// field-measured collision (see the embedded-dolt-one-readwrite-engine-per-path
// project memory: 6 of 15 rapid foreground commands failed this way against a
// concurrent background writer). This test drives the same shape — several
// `lit new` calls fired in immediate succession — end to end through the real
// CLI binary rather than the store package directly (see
// TestConcurrentOpenWaitsForLiveWriteEngine and
// TestOpenSyncWaitsForLiveForegroundEngine in
// internal/store/engine_serialization_test.go for the deterministic,
// non-timing-dependent proof of the underlying mechanism this test
// complements).
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

	// Isolate this run's config from whatever XDG config exists on the host,
	// same as TestEagerPushOnDefaultCadenceReachesRemoteWithoutExplicitPush:
	// this test's claim is about the shipped default cadence, not whatever a
	// developer's machine happens to have configured — passed per-call via
	// isolatedEnv, never t.Setenv, so the test stays parallel-safe.
	// [LAW:no-ambient-temporal-coupling]
	xdgConfigHome := filepath.Join(base, "xdg-config")
	if err := os.Mkdir(xdgConfigHome, 0o755); err != nil {
		t.Fatalf("mkdir xdg config home: %v", err)
	}

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

	if out, err := runLit(t, root, self, isolatedEnv(xdgConfigHome, "1"),
		"init", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out)
	}
	// The oracle poll below proves DELIVERY, not quiescence: a mirror can
	// still owe a post-release re-check cycle (a claim stamped after the
	// delivering cycle's entry-clear) when the commit count is already
	// satisfied, and that no-op cycle's engine would race this test's TempDir
	// sweep. The quiescence cleanup owns TempDir safety — it holds the
	// single-flight lock through the sweep, so an in-cycle mirror is waited
	// out and a pre-lock one exits silently. [LAW:no-ambient-temporal-coupling]
	awaitMirrorQuiescence(t, root)
	if out, err := runLit(t, root, self, isolatedEnv(xdgConfigHome, "1"),
		"sync", "push", "--set-upstream"); err != nil {
		t.Fatalf("bootstrap lit sync push: %v\noutput:\n%s", err, out)
	}

	// Independent oracle, same technique as
	// TestEagerPushOnDefaultCadenceReachesRemoteWithoutExplicitPush: a plain
	// `dolt clone` of the bare remote, touched by nothing else in this test.
	ref := "remotes/origin/" + branch
	oracleDir := filepath.Join(base, "oracle-clone")
	runDolt(t, base, "clone", "file://"+remoteGit, oracleDir)
	baseline := doltRefCommitCount(t, oracleDir, ref)

	// No config.toml: this run exercises the shipped default (on-change), the
	// same way a fresh agent workspace does — and, unlike the bootstrap calls
	// above, does NOT set disableAutoSyncEnvVar, so every mutation below
	// spawns a real on-change mirror exactly as production does.
	const burstSize = 15
	for i := 0; i < burstSize; i++ {
		out, err := runLit(t, root, self, isolatedEnv(xdgConfigHome, "0"),
			"new", "--title", "burst-issue", "--topic", "demo")
		if err != nil {
			t.Fatalf("lit new #%d failed: %v\noutput:\n%s\n%s", i, err, out, dumpMirrorLog(root))
		}
		lower := strings.ToLower(out)
		if strings.Contains(lower, "database is read only") || strings.Contains(lower, "cannot update manifest") {
			t.Fatalf("lit new #%d hit an engine open-collision instead of waiting for it to clear:\noutput:\n%s\n%s", i, out, dumpMirrorLog(root))
		}
		if strings.Contains(lower, "online garbage collection") && strings.Contains(lower, "reconnect") {
			t.Fatalf("lit new #%d hit an online-GC reconnect collision:\noutput:\n%s\n%s", i, out, dumpMirrorLog(root))
		}
	}

	// No sweep push. The oracle poll below alone proving delivery IS the
	// acceptance for links-sync-pgct.12: the burst's FINAL mutation either
	// observed a fresh mirror-pending claim (a spawned, not-yet-cleared
	// mirror whose engine open — and so HEAD read — still lies ahead of the
	// mutation's closed session) or claimed the marker and spawned its own
	// mirror. Under the old 1s spawn debounce that tail was a timing bet, and
	// this test needed an explicit `lit sync push` sweep to be deterministic;
	// a sweep now would mask a regression in exactly the guarantee this test
	// pins. [LAW:no-ambient-temporal-coupling] the invariant is owned state
	// (the mirror-pending marker), not a time window, so the poll may bet on
	// it.
	//
	// The poll proves delivery only; TempDir safety against a mirror that
	// outlives the satisfied commit count (a post-release re-check cycle) is
	// owned by the awaitMirrorQuiescence cleanup registered above.
	//
	// Budget: delivery of the tail can legitimately chain up to three full
	// mirror cycles (the in-flight push finishing, the holder's post-release
	// re-check cycle, and one more if a claim lands mid-cycle), each a real
	// engine open plus a push. Generous headroom for a loaded CI machine
	// costs nothing when healthy — the poll returns the moment the count
	// matches.
	const settleTimeout = 60 * time.Second
	const settlePollInterval = 300 * time.Millisecond
	deadline := time.Now().Add(settleTimeout)
	for {
		if _, err := tryDolt(oracleDir, "fetch", "origin"); err == nil {
			if doltRefCommitCount(t, oracleDir, ref) >= baseline+burstSize {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("burst's %d commits never all reached the remote within %s of the last lit new returning\n%s", burstSize, settleTimeout, dumpMirrorLog(root))
		}
		time.Sleep(settlePollInterval)
	}
}

// dumpMirrorLog reads the on-change mirror's durable log (mirrorLogName in
// internal/cli/sync_bg.go) at its conventional path — <git-common-dir>/links/
// mirror.log, i.e. <root>/.git/links/mirror.log for a plain (non-worktree)
// repo, matching internal/workspace.deriveLocation's StorageDir derivation —
// and formats it for inclusion in a failure message. Every mirror this test's
// foreground commands spawn writes here, so on a failure this is the one place
// that can show what a background mirror (parent-wait, single-flight,
// engine-open wait, push) was actually doing when the failure happened,
// instead of guessing from the foreground command's error alone.
func dumpMirrorLog(root string) string {
	path := filepath.Join(root, ".git", "links", "mirror.log")
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("mirror log (%s) unavailable: %v", path, err)
	}
	if len(content) == 0 {
		return fmt.Sprintf("mirror log (%s) is empty", path)
	}
	return fmt.Sprintf("mirror log (%s):\n%s", path, content)
}
