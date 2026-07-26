//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// reexecEnvVar, when set on a child invocation of this test binary, makes
// TestMain run the real lit main() instead of the test suite. This lets the
// SIGTERM acceptance test drive the ACTUAL entrypoint — interrupt.Guard, cli.Run,
// the real post-write sync — as a separate OS process it can signal, without a
// separate `go build` step. [LAW:behavior-not-structure] the test exercises the
// shipped binary's behavior, not a test-only reimplementation of it.
const reexecEnvVar = "LIT_TEST_REEXEC"

// disableAutoSyncEnvVar is the process-level auto-sync kill switch, read from the
// one canonical definition in internal/cli so this test cannot drift from the
// CLI's env contract if that name ever changes. [LAW:one-source-of-truth]
const disableAutoSyncEnvVar = cli.DisableAutoSyncEnvVar

func TestMain(m *testing.M) {
	if os.Getenv(reexecEnvVar) == "1" {
		// Behave as the real lit binary. main() owns its own os.Exit on the error
		// and escalation paths; a clean return here means the command succeeded.
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestSIGTERMDuringWedgedSyncExitsCleanly is the acceptance pin for
// links-sync-s3r6 defect #1: a SIGTERM delivered while the POST-WRITE auto-sync
// is wedged must cancel that phase, release the store, and exit promptly with the
// write's own success code (0) — never sit until only SIGKILL ends it.
//
// The wedge targets the sync phase specifically, not the command's own work. A
// `lit new` write acquires and RELEASES the commit lock to commit, then prints
// the created-issue line, then (after ap.Close) the inline receive re-acquires
// the lock at SyncAddRemote. Grabbing the lock with a live foreign process the
// instant that line appears lands the block in the receive — the write already
// succeeded and is durable — so a clean cancel exits 0, exactly the incident's
// "commit present, only the sync wedged" shape, reproduced without a slow remote.
func TestSIGTERMDuringWedgedSyncExitsCleanly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	ws := setupWedgeWorkspace(t, self)
	lockPath := store.CommitLockPath(ws.DatabasePath)

	// A live process whose PID will own the commit lock: fresh and running, so the
	// store cannot classify it as stale and reclaim it — the receive must wait.
	holder := exec.Command("sleep", "300")
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	defer func() { _ = holder.Process.Kill(); _, _ = holder.Process.Wait() }()

	cmd := exec.Command(self, "new", "--title", "wedge-me", "--topic", "demo")
	cmd.Dir = ws.RootDir
	cmd.Env = litEnv(map[string]string{disableAutoSyncEnvVar: "0"})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wedged command: %v", err)
	}

	// The created-issue line is printed after the write has committed and released
	// the lock, but before ap.Close and the receive. Seizing the lock here — well
	// inside the ~hundreds-of-ms it takes the receive to reach SyncAddRemote —
	// wedges the receive, not the (already durable) write.
	wroteLine := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			close(wroteLine)
		}
		_, _ = io.Copy(io.Discard, stdout) // drain so the child never blocks on a full pipe
	}()

	select {
	case <-wroteLine:
	case <-time.After(15 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatalf("lit new never printed its created-issue line:\n%s", stderr.String())
	}
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", holder.Process.Pid)), 0o600); err != nil {
		t.Fatalf("seize commit lock: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	// The receive must actually reach the seized lock and block: the process has to
	// still be running after a settle window. If it already exited, the wedge never
	// engaged (the receive passed SyncAddRemote before the seize) and the exit-0
	// below would be a false pass, not proof of SIGTERM-responsiveness.
	select {
	case err := <-waitCh:
		t.Fatalf("command exited before the wedge engaged (err=%v); the receive did not block on the seized lock:\nstderr:\n%s", err, stderr.String())
	case <-time.After(1 * time.Second):
	}

	sentAt := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	const sigtermDeadline = 8 * time.Second // comfortably under the 15s inline-receive budget
	select {
	case err := <-waitCh:
		if elapsed := time.Since(sentAt); elapsed > sigtermDeadline {
			t.Fatalf("process took %v to exit after SIGTERM (want < %v)", elapsed, sigtermDeadline)
		}
		// The write succeeded before the wedge; the sync is best-effort, so a
		// cancelled sync must not turn the successful write into a failure exit.
		if err != nil {
			t.Fatalf("SIGTERM-ed sync wedge exited non-zero (want the write's success code 0): %v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(sigtermDeadline):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatalf("process did not exit within %v of SIGTERM — the sync phase is not SIGTERM-responsive:\nstderr:\n%s", sigtermDeadline, stderr.String())
	}

	// The store was released and lit stranded no lock of its own: with the foreign
	// holder gone, an ordinary write proceeds normally.
	_ = holder.Process.Kill()
	_, _ = holder.Process.Wait()
	_ = os.Remove(lockPath)
	verifyOut, err := runLit(t, ws.RootDir, self, map[string]string{disableAutoSyncEnvVar: "1"},
		"new", "--title", "after-wedge", "--topic", "demo")
	if err != nil {
		t.Fatalf("workspace not usable after the SIGTERM-ed sync: %v\noutput:\n%s", err, verifyOut)
	}
}

// setupWedgeWorkspace builds a git repo with a configured (never-pushed) remote
// and an initialized lit store, and returns its resolved workspace info. The
// remote's mere presence is what drives the first inline receive to call
// SyncAddRemote — the commit-lock mutation the test wedges.
func setupWedgeWorkspace(t *testing.T, self string) workspace.Info {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "wedge@test.co")
	runGit(t, root, "config", "user.name", "wedge")
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-m", "seed")
	runGit(t, base, "init", "--bare", "remote.git")
	runGit(t, root, "remote", "add", "origin", filepath.Join(base, "remote.git"))

	if out, err := runLit(t, root, self, map[string]string{disableAutoSyncEnvVar: "1"},
		"init", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out)
	}

	ws, err := workspace.Resolve(root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	return ws
}

// runLit runs a lit command to completion via a re-exec of the test binary.
func runLit(t *testing.T, dir, self string, extraEnv map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(self, args...)
	cmd.Dir = dir
	cmd.Env = litEnv(extraEnv)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// litEnv builds the child environment: the parent's, with the re-exec marker set
// and the caller's overrides applied, de-duplicated so getenv reads the intended
// value regardless of platform lookup order.
func litEnv(extra map[string]string) []string {
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			merged[kv[:i]] = kv[i+1:]
		}
	}
	merged[reexecEnvVar] = "1"
	for k, v := range extra {
		merged[k] = v
	}
	env := make([]string, 0, len(merged))
	for k, v := range merged {
		env = append(env, k+"="+v)
	}
	return env
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestSIGTERMDuringWedgedGitSubprocessExitsCleanly is the acceptance pin for
// links-sync-srox: a SIGTERM delivered while the post-write auto-sync is wedged in
// a git SUBPROCESS — an ls-remote to an unreachable remote during the receive's
// first-push check — must cancel that subprocess and exit with the write's own
// success code (0), not sit out the interrupt grace timer and hard-exit 143.
//
// This is the sibling of TestSIGTERMDuringWedgedSyncExitsCleanly, which wedges the
// store's commit lock (a wait that already honored ctx). Here the wedge is the git
// call that formerly shelled out with NO context: before the fix, cancelling the
// root context left `git ls-remote` running and only interrupt's grace-timer
// hard-exit (code 143) stopped the process. The clean path now kills the subprocess
// on cancellation and lets main() exit with the write's 0.
//
// The remote is a black-hole TCP listener: it accepts git's connection and never
// answers the ref advertisement, so `git ls-remote origin` blocks in git itself —
// no transport subprocess, so cancelling the command kills git and unblocks its
// stdout read at once (an ext-transport hang would leave a grandchild holding the
// pipe and defeat the test).
func TestSIGTERMDuringWedgedGitSubprocessExitsCleanly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	remoteURL := blackHoleGitRemote(t)
	ws := setupGitWedgeWorkspace(t, self, remoteURL)

	cmd := exec.Command(self, "new", "--title", "wedge-me", "--topic", "demo")
	cmd.Dir = ws.RootDir
	cmd.Env = litEnv(map[string]string{disableAutoSyncEnvVar: "0"})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wedged command: %v", err)
	}

	// The created-issue line prints after the write commits and before the inline
	// receive runs, so its arrival means the durable write is done and the receive —
	// which will block on the black-hole ls-remote — is about to start.
	wroteLine := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			close(wroteLine)
		}
		_, _ = io.Copy(io.Discard, stdout) // drain so the child never blocks on a full pipe
	}()

	select {
	case <-wroteLine:
	case <-time.After(15 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatalf("lit new never printed its created-issue line:\n%s", stderr.String())
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	// The receive must actually reach the black-hole ls-remote and block: the process
	// has to still be running after a settle window. If it already exited, the wedge
	// never engaged and the exit-0 below would be a false pass.
	select {
	case err := <-waitCh:
		t.Fatalf("command exited before the git wedge engaged (err=%v); the receive did not block on the hung ls-remote:\nstderr:\n%s", err, stderr.String())
	case <-time.After(1 * time.Second):
	}

	sentAt := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	// Deliberately UNDER interrupt.DefaultGrace (5s): a clean ctx-cancel exit is
	// milliseconds, while the pre-fix behavior — git ignoring the cancel — only ends
	// at the grace-timer hard-exit (143) at ~5s. A deadline below grace fails the
	// old path on BOTH counts (too slow AND non-zero), leaving no way for a
	// grace-timer exit to masquerade as success.
	const sigtermDeadline = 4 * time.Second
	select {
	case err := <-waitCh:
		if elapsed := time.Since(sentAt); elapsed > sigtermDeadline {
			t.Fatalf("process took %v to exit after SIGTERM (want < %v) — the git subprocess did not honor cancellation", elapsed, sigtermDeadline)
		}
		// The write succeeded before the wedge; the sync is best-effort, so a
		// cancelled sync must not turn the successful write into a failure exit.
		if err != nil {
			t.Fatalf("SIGTERM-ed git wedge exited non-zero (want the write's success code 0; 143 means the grace timer hard-exited a still-running git): %v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(sigtermDeadline):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatalf("process did not exit within %v of SIGTERM — the sync-phase git subprocess is not cancellation-responsive:\nstderr:\n%s", sigtermDeadline, stderr.String())
	}

	// The store was released and lit stranded no lock of its own: with the black-hole
	// remote removed, an ordinary write proceeds normally.
	runGit(t, ws.RootDir, "remote", "remove", "origin")
	verifyOut, err := runLit(t, ws.RootDir, self, map[string]string{disableAutoSyncEnvVar: "1"},
		"new", "--title", "after-wedge", "--topic", "demo")
	if err != nil {
		t.Fatalf("workspace not usable after the SIGTERM-ed git wedge: %v\noutput:\n%s", err, verifyOut)
	}
}

// blackHoleGitRemote starts a TCP listener that accepts connections and never
// responds, and returns a git:// URL pointing at it. `git ls-remote` against this
// URL completes its TCP connect, sends its request, then blocks forever waiting for
// the ref advertisement — a deterministic, offline network hang with no transport
// subprocess of its own. The listener is closed on test cleanup.
func blackHoleGitRemote(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for black-hole remote: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed on cleanup
			}
			// Hold the connection open and never write the git ref advertisement.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	return "git://127.0.0.1:" + strconv.Itoa(port) + "/wedge.git"
}

// setupGitWedgeWorkspace builds a git repo with an initialized lit store, then
// points origin at a black-hole remote AFTER init so init's own remote probes never
// touch it — only the post-`lit new` auto-sync does. Returns the resolved workspace.
func setupGitWedgeWorkspace(t *testing.T, self, remoteURL string) workspace.Info {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "wedge@test.co")
	runGit(t, root, "config", "user.name", "wedge")
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-m", "seed")

	if out, err := runLit(t, root, self, map[string]string{disableAutoSyncEnvVar: "1"},
		"init", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out)
	}

	// Add the black-hole remote only now — the first `lit new` auto-sync is the one
	// that reaches the wedged ls-remote.
	runGit(t, root, "remote", "add", "origin", remoteURL)

	ws, err := workspace.Resolve(root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	return ws
}
