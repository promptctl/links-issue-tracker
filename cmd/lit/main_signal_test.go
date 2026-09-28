//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/trace"
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

// TestSIGTERMDuringWedgedSyncExitsCleanly is the acceptance pin: a SIGTERM
// delivered while the automatic receive is wedged on the store's commit lock
// must cancel it and end the receive worker promptly and cleanly — never leave
// a process only SIGKILL ends.
//
// The command itself does not wait for the receive at all: it spawns the
// detached worker and returns, so the wedge is planted before the command runs
// and it still returns. A read takes no commit lock, so the held lock wedges
// only the worker, where its clone of the store takes the commit lock. (The
// kernel excludes the worker on the held flock no matter who the holder is, so
// no foreign holder process is needed.)
func TestSIGTERMDuringWedgedSyncExitsCleanly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	ws, cadenceConfig := setupWedgeWorkspace(t, self)
	// One write first, so the store is past its baseline and the read below
	// needs no write open of its own — only the worker is left to wedge.
	if out, err := runLit(t, ws.RootDir, self, onPushEnv(cadenceConfig, "1"),
		"new", "--title", "before-wedge", "--topic", "demo"); err != nil {
		t.Fatalf("lit new before the wedge: %v\noutput:\n%s", err, out)
	}

	seizeCtx, cancelSeize := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSeize()
	releaseSeize, err := store.LockCommitPath(seizeCtx, ws.DatabasePath)
	if err != nil {
		t.Fatalf("seize commit lock: %v", err)
	}
	seizeHeld := true
	defer func() {
		if seizeHeld {
			_ = releaseSeize()
		}
	}()

	pid := commandReturnsAheadOfItsReceive(t, ws, self, cadenceConfig)

	// The worker has to reach the seized lock and block there: still running
	// after a settle window, well past the milliseconds it needs to ask the
	// remote. A worker already gone never engaged the wedge, and its clean end
	// below would prove nothing.
	time.Sleep(2 * time.Second)
	if !processAlive(pid) {
		t.Fatalf("receive worker %d ended before the wedge engaged:\n%s", pid, receiveLog(t, ws))
	}
	sigtermReceiveWorker(t, ws, pid, 8*time.Second) // comfortably under the receive's 15s deadline

	// The receive's one recorded failure is the cancellation reaching its clone
	// of the store. A worker that was never at the clone, or one that ignored
	// the cancel and ended when the commit lock's own wait gave up — also on
	// the clean path, end line and all — records something else.
	reasons := receiveTraceReasons(t, ws)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "clone") ||
		!strings.HasSuffix(reasons[0], context.Canceled.Error()) {
		t.Fatalf("want one receive trace, the clone take cancelled; got %q", reasons)
	}

	// The store was released and lit stranded no hold of its own: with the
	// seizing flock released, an ordinary write proceeds normally.
	seizeHeld = false
	if err := releaseSeize(); err != nil {
		t.Fatalf("release seized commit lock: %v", err)
	}
	verifyOut, err := runLit(t, ws.RootDir, self, onPushEnv(cadenceConfig, "1"),
		"new", "--title", "after-wedge", "--topic", "demo")
	if err != nil {
		t.Fatalf("workspace not usable after the SIGTERM-ed receive: %v\noutput:\n%s", err, verifyOut)
	}
}

// commandReturnsAheadOfItsReceive runs `lit backlog` with automatic sync on and
// the receive due, which is what makes it spawn the receive worker, and
// returns that worker's pid once it has shown it is running. The command must
// have exited by then while the worker has not ended: whatever the receive is
// wedged on, the command did not wait for it.
func commandReturnsAheadOfItsReceive(t *testing.T, ws workspace.Info, self, cadenceConfig string) int {
	t.Helper()
	out, err := runLit(t, ws.RootDir, self, onPushEnv(cadenceConfig, "0"), "backlog")
	if err != nil {
		t.Fatalf("lit backlog: %v\noutput:\n%s", err, out)
	}
	pid := receiveWorkerPID(t, ws)
	if strings.Contains(receiveLog(t, ws), fmt.Sprintf("receive end pid=%d ", pid)) {
		t.Fatalf("the receive had already ended when the command returned, so this wedge did not hold it:\n%s", receiveLog(t, ws))
	}
	return pid
}

// receiveWorkerPID waits for the receive worker's start line and returns the
// pid it names. The worker writes it the moment it starts, which can be just
// after the command that spawned it has exited.
func receiveWorkerPID(t *testing.T, ws workspace.Info) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, line := range strings.Split(receiveLog(t, ws), "\n") {
			if _, rest, ok := strings.Cut(line, " receive start pid="); ok {
				pid, err := strconv.Atoi(strings.Fields(rest)[0])
				if err != nil {
					t.Fatalf("unparseable receive start line %q: %v", line, err)
				}
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no receive worker started within 15s:\n%s", receiveLog(t, ws))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sigtermReceiveWorker sends SIGTERM to the receive worker and requires it to
// be gone within deadline, having written its end line: the end line is
// written only on the clean path, after the receive returned, so a worker
// the interrupt grace timer had to hard-exit leaves none.
func sigtermReceiveWorker(t *testing.T, ws workspace.Info, pid int, deadline time.Duration) {
	t.Helper()
	sentAt := time.Now()
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to receive worker %d: %v", pid, err)
	}
	for processAlive(pid) {
		if time.Since(sentAt) > deadline {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("receive worker %d did not exit within %v of SIGTERM — the receive is not cancellation-responsive:\n%s", pid, deadline, receiveLog(t, ws))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(receiveLog(t, ws), fmt.Sprintf("receive end pid=%d ", pid)) {
		t.Fatalf("receive worker %d exited without its end line — it did not end on the clean path:\n%s", pid, receiveLog(t, ws))
	}
}

// processAlive reports whether pid names a live process. The detached worker
// is reparented when its command exits, and its new parent reaps it, so a
// finished worker stops answering rather than lingering as a zombie.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// receiveTraceReasons returns the reason of every sync trace the automatic
// receive recorded in ws, in the order they were written.
func receiveTraceReasons(t *testing.T, ws workspace.Info) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(trace.Dir(ws.StorageDir, "sync"), "*.json"))
	if err != nil {
		t.Fatalf("list sync traces: %v", err)
	}
	var reasons []string
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read sync trace: %v", err)
		}
		var record struct {
			Command string `json:"command"`
			Reason  string `json:"reason"`
		}
		if err := json.Unmarshal(body, &record); err != nil {
			t.Fatalf("parse sync trace %s: %v", path, err)
		}
		if record.Command == "lit sync receive" {
			reasons = append(reasons, record.Reason)
		}
	}
	return reasons
}

func receiveLog(t *testing.T, ws workspace.Info) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(ws.StorageDir, cli.ReceiveLogName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read receive log: %v", err)
	}
	return string(body)
}

// setupWedgeWorkspace builds a git repo with a remote carrying lit data and an
// initialized lit store, and returns its resolved workspace info plus the
// cadence-pin config path for the caller's own child invocations. The data on
// the remote, with no record of having received it, is what drives the first
// automatic receive to clone the store — the commit-lock hold the test wedges.
func setupWedgeWorkspace(t *testing.T, self string) (workspace.Info, string) {
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

	cadenceConfig := pinOnPushCadence(t, base)
	if out, err := runLit(t, root, self, onPushEnv(cadenceConfig, "1"),
		"init", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out)
	}

	ws, err := workspace.Resolve(root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	// The remote must carry lit data, or the receive's ask finds nothing to
	// fetch and ends before it touches the store — there would be nothing to
	// wedge. The push records the advertisement it left, which would make the
	// ask answer "unmoved"; removing that record makes the receive fetch.
	runGit(t, root, "push", "-u", "origin", "HEAD")
	if out, err := runLit(t, root, self, onPushEnv(cadenceConfig, "1"),
		"sync", "push", "--set-upstream"); err != nil {
		t.Fatalf("seed lit sync push: %v\noutput:\n%s", err, out)
	}
	if err := os.Remove(store.ReceivedRefsPath(ws.DatabasePath)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove the push's received-refs record: %v", err)
	}
	return ws, cadenceConfig
}

// pinOnPushCadence writes a config.toml selecting on-push cadence under dir
// and returns its path, for the caller to hand every child process via
// onPushEnv. The path travels as an explicit per-call override rather than a
// t.Setenv mutation of this process — the variable is consumed only by the
// child lit processes, and keeping the shared process environment untouched is
// what lets the wedge tests run in parallel with the rest of the package.
//
// The SIGTERM wedge tests are specifically about the AUTOMATIC RECEIVE; the
// on-change cadence's background push mirror is an orthogonal automatic
// behavior that adds a second async actor racing these tests' wedge and
// verification steps on the store's single read-write engine — a flake these
// tests are not designed to account for. Both wedge tests pin cadence
// explicitly instead of depending on whatever value happens to be the shipped
// default. [LAW:locality-or-seam]
func pinOnPushCadence(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "wedge-test-config.toml")
	if err := os.WriteFile(path, []byte("[sync]\ncadence = \"on-push\"\n"), 0o644); err != nil {
		t.Fatalf("write cadence-pin config: %v", err)
	}
	return path
}

// onPushEnv builds the child-env overrides for a wedge test: the cadence-pin
// config written by pinOnPushCadence, plus the auto-sync switch.
func onPushEnv(cadenceConfigPath, disableAutoSync string) map[string]string {
	return map[string]string{
		"LIT_CONFIG_GLOBAL_PATH": cadenceConfigPath,
		disableAutoSyncEnvVar:    disableAutoSync,
	}
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

// TestSIGTERMDuringWedgedGitSubprocessExitsCleanly is the acceptance pin: with
// the remote hung — a `git ls-remote` that never gets its ref advertisement —
// `lit backlog` with the receive due still returns, because the question runs
// in the detached receive worker, never in the command. And a SIGTERM
// delivered to that worker while it is wedged in the git SUBPROCESS must
// cancel the subprocess and end the worker cleanly, not sit out the interrupt
// grace timer.
//
// The remote is a black-hole TCP listener: it accepts git's connection and never
// answers the ref advertisement, so `git ls-remote origin` blocks in git itself —
// no transport subprocess, so cancelling kills git and unblocks its stdout read
// at once (an ext-transport hang would leave a grandchild holding the pipe and
// defeat the test). Its accept count is how the test knows the wedge engaged.
func TestSIGTERMDuringWedgedGitSubprocessExitsCleanly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	remoteURL, connected := blackHoleGitRemote(t)
	ws, cadenceConfig := setupGitWedgeWorkspace(t, self, remoteURL)

	pid := commandReturnsAheadOfItsReceive(t, ws, self, cadenceConfig)

	// The worker must actually reach the black-hole ls-remote and block there.
	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatalf("the receive worker never reached the remote:\n%s", receiveLog(t, ws))
	}
	time.Sleep(200 * time.Millisecond) // git has sent its request and is waiting on the reply
	if !processAlive(pid) {
		t.Fatalf("receive worker %d ended before the git wedge could be signalled:\n%s", pid, receiveLog(t, ws))
	}

	// Deliberately UNDER interrupt.DefaultGrace (5s): a clean ctx-cancel exit is
	// milliseconds, while git ignoring the cancel only ends at the grace-timer
	// hard-exit at ~5s, which also skips the end line. A deadline below grace
	// fails that path on both counts.
	sigtermReceiveWorker(t, ws, pid, 4*time.Second)

	// The store was released and lit stranded no lock of its own: with the black-hole
	// remote removed, an ordinary write proceeds normally.
	runGit(t, ws.RootDir, "remote", "remove", "origin")
	verifyOut, err := runLit(t, ws.RootDir, self, onPushEnv(cadenceConfig, "1"),
		"new", "--title", "after-wedge", "--topic", "demo")
	if err != nil {
		t.Fatalf("workspace not usable after the SIGTERM-ed git wedge: %v\noutput:\n%s", err, verifyOut)
	}
}

// TestAPeerPushReachesTheStoreThroughTheReceiveWorker is the end-to-end proof
// that the detached receive still receives (links-scale-om3r.6cv): a peer
// pushes a ticket, one command here with the receive due spawns the worker and
// returns, and once that worker ends the store holds the peer's ticket, with
// no `lit sync pull`.
func TestAPeerPushReachesTheStoreThroughTheReceiveWorker(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}
	base := t.TempDir()
	cadenceConfig := pinOnPushCadence(t, base)
	quiet := onPushEnv(cadenceConfig, "1")
	lit := func(dir string, env map[string]string, args ...string) string {
		t.Helper()
		out, err := runLit(t, dir, self, env, args...)
		if err != nil {
			t.Fatalf("lit %s in %s: %v\noutput:\n%s", strings.Join(args, " "), dir, err, out)
		}
		return out
	}

	runGit(t, base, "init", "--bare", "remote.git")
	producer := filepath.Join(base, "alpha")
	runGit(t, base, "clone", filepath.Join(base, "remote.git"), "alpha")
	runGit(t, producer, "config", "user.email", "a@a.co")
	runGit(t, producer, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(producer, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, producer, "add", "-A")
	runGit(t, producer, "commit", "-m", "seed")
	runGit(t, producer, "push", "origin", "HEAD")
	lit(producer, quiet, "init", "--skip-hooks", "--skip-agents")
	lit(producer, quiet, "new", "--title", "first-ticket", "--topic", "demo", "--type", "task")
	lit(producer, quiet, "sync", "push", "--set-upstream")

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", filepath.Join(base, "remote.git"), "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	lit(consumer, quiet, "init", "--skip-hooks", "--skip-agents")
	ws, err := workspace.Resolve(consumer)
	if err != nil {
		t.Fatalf("resolve consumer workspace: %v", err)
	}

	lit(producer, quiet, "new", "--title", "second-ticket", "--topic", "demo", "--type", "task")
	lit(producer, quiet, "sync", "push")
	if strings.Contains(lit(consumer, quiet, "backlog"), "second-ticket") {
		t.Fatalf("consumer saw second-ticket before any receive — the test cannot prove the receive")
	}

	lit(consumer, onPushEnv(cadenceConfig, "0"), "backlog")
	pid := receiveWorkerPID(t, ws)
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(receiveLog(t, ws), fmt.Sprintf("receive end pid=%d ", pid)) {
		if time.Now().After(deadline) {
			t.Fatalf("receive worker %d did not end within 60s:\n%s", pid, receiveLog(t, ws))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if backlog := lit(consumer, quiet, "backlog"); !strings.Contains(backlog, "second-ticket") {
		t.Fatalf("consumer backlog missing the peer's second-ticket after the receive worker ended:\n%s\nreceive log:\n%s", backlog, receiveLog(t, ws))
	}
}

// blackHoleGitRemote starts a TCP listener that accepts connections and never
// responds, and returns a git:// URL pointing at it. `git ls-remote` against this
// URL completes its TCP connect, sends its request, then blocks forever waiting for
// the ref advertisement — a deterministic, offline network hang with no transport
// subprocess of its own. connected closes on the first connection it accepts. The
// listener is closed on test cleanup.
func blackHoleGitRemote(t *testing.T) (url string, connected <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for black-hole remote: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{})
	go func() {
		var once sync.Once
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed on cleanup
			}
			once.Do(func() { close(accepted) })
			// Hold the connection open and never write the git ref advertisement.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	return "git://127.0.0.1:" + strconv.Itoa(port) + "/wedge.git", accepted
}

// setupGitWedgeWorkspace builds a git repo with an initialized lit store, then
// points origin at a black-hole remote AFTER init so init's own remote probes never
// touch it — only the automatic receive does. Returns the resolved workspace.
func setupGitWedgeWorkspace(t *testing.T, self, remoteURL string) (workspace.Info, string) {
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

	cadenceConfig := pinOnPushCadence(t, base)
	if out, err := runLit(t, root, self, onPushEnv(cadenceConfig, "1"),
		"init", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out)
	}

	// Add the black-hole remote only now — the first automatic receive is the one
	// that reaches the wedged ls-remote. Record its default branch locally, as a
	// clone does, so the command's read resolves its sync branch without asking
	// the remote (links-scale-om3r.06l is that ask).
	runGit(t, root, "remote", "add", "origin", remoteURL)
	runGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")

	ws, err := workspace.Resolve(root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	return ws, cadenceConfig
}
