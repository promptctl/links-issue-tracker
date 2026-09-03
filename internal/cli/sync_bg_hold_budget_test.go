//go:build !windows

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// lockedBuffer is a bytes.Buffer whose every access holds one mutex, so the
// test can read output while an abandoned in-process Run may still write it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestMirrorHoldBudgetCutsHungPushAndReleasesEngine is the regression pin for
// links-sync-pgct.11.1: the background mirror's push shells out to git while
// this process holds the store's one read-write engine (and its journal lock),
// and before the fix NOTHING bounded that hold — a hung transport held the
// lock for as long as the remote cared to stall, starving every foreground
// command past its ~30s open-retry budget.
//
// The test reproduces the exact field topology with no network: a git shim on
// PATH wedges `git push` (the engine-side subprocess DOLT_PUSH spawns) in an
// indefinite sleep, and the mirror's hidden subcommand runs in-process against
// a seeded git+file remote with a real unpushed commit. The fix must
// (a) cut the whole cycle at store.MirrorHoldBudget — which requires the
// deadline to travel from the session-open ctx through the embedded driver's
// connection context into the git subprocess's kill — and (b) leave the engine
// released, proven by a foreground mutation succeeding immediately after.
// A wedge marker written by the shim pins that the push was genuinely mid-hang
// when the budget fired, so the pass cannot come from a push that
// short-circuited before touching git.
//
// Not parallel: it mutates PATH (t.Setenv) and store.MirrorHoldBudget.
func TestMirrorHoldBudgetCutsHungPushAndReleasesEngine(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "hold-budget@test.co")
	runGit(t, root, "config", "user.name", "hold-budget")
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-m", "seed")

	remoteGit := filepath.Join(base, "remote.git")
	runGit(t, base, "init", "--bare", "remote.git")
	runGit(t, root, "remote", "add", "origin", remoteGit)
	runGit(t, root, "push", "-u", "origin", "HEAD")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir(root) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })
	t.Setenv("HOME", base)
	t.Setenv("CODEX_HOME", filepath.Join(base, ".codex-home"))

	// The timeout branch reads the buffer while Run's goroutine may still be
	// writing it, so every access goes through one lock.
	runInProcess := func(timeout time.Duration, args ...string) (*lockedBuffer, error) {
		out := &lockedBuffer{}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		errCh := make(chan error, 1)
		go func() { errCh <- Run(ctx, out, out, args) }()
		select {
		case runErr := <-errCh:
			return out, runErr
		case <-ctx.Done():
			t.Fatalf("Run(%v) still blocked after %s — the hold is unbounded:\noutput:\n%s", args, timeout, out.String())
			return out, nil
		}
	}

	if out, err := runInProcess(60*time.Second, "init", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out.String())
	}
	// First dolt push establishes the remote's dolt ref and upstream, with real
	// git, so the wedged run below is the mirror's ordinary incremental push.
	if out, err := runInProcess(60*time.Second, "sync", "push", "--set-upstream"); err != nil {
		t.Fatalf("bootstrap lit sync push: %v\noutput:\n%s", err, out.String())
	}
	// A real unpushed commit, so the mirror's push has work and must reach the
	// git subprocess rather than short-circuiting as up-to-date.
	if out, err := runInProcess(60*time.Second, "new", "--title", "hold-budget probe", "--topic", "demo"); err != nil {
		t.Fatalf("lit new (pre-wedge): %v\noutput:\n%s", err, out.String())
	}

	// The shim wedges only `git push` — every other git call (ls-remote, fetch,
	// the workspace's own plumbing) passes through to the real binary — and
	// stamps a marker first, so the assertions below can distinguish "the budget
	// cut a genuinely hung subprocess" from "the push never ran".
	wedgeMarker := filepath.Join(base, "wedge-engaged")
	shimDir := filepath.Join(base, "git-shim")
	if err := os.Mkdir(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim dir: %v", err)
	}
	shim := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  case "$a" in
    push)
      : > %q
      exec /bin/sleep 600
      ;;
  esac
done
exec %q "$@"
`, wedgeMarker, gitPath)
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	restoreBudget := store.MirrorHoldBudget
	store.MirrorHoldBudget = 8 * time.Second
	t.Cleanup(func() { store.MirrorHoldBudget = restoreBudget })

	// The mirror's own entrypoint, in the foreground: parent-pid 0 skips the
	// parent wait, then the cycle opens the engine and pushes into the wedge.
	started := time.Now()
	mirrorOut, mirrorErr := runInProcess(90*time.Second, "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
	elapsed := time.Since(started)
	if mirrorErr != nil {
		t.Fatalf("mirror run returned error (the mirror is best-effort and must exit 0): %v\noutput:\n%s", mirrorErr, mirrorOut.String())
	}
	if _, err := os.Stat(wedgeMarker); err != nil {
		t.Fatalf("the wedge never engaged (marker missing: %v) — the push short-circuited and the test proved nothing:\noutput:\n%s", err, mirrorOut.String())
	}
	if elapsed > 30*time.Second {
		t.Fatalf("mirror held its session %s against a hung push (budget %s) — the hold bound is not working:\noutput:\n%s", elapsed, store.MirrorHoldBudget, mirrorOut.String())
	}
	if !strings.Contains(mirrorOut.String(), "hold_budget_cut=true") {
		t.Fatalf("cycle log does not report the budget cut:\noutput:\n%s", mirrorOut.String())
	}

	// The engine (and journal lock) must be free: a foreground mutation opens
	// and commits without waiting out any residue of the wedged push.
	if out, err := runInProcess(30*time.Second, "new", "--title", "post-wedge probe", "--topic", "demo"); err != nil {
		t.Fatalf("foreground mutation after the cut mirror failed — the engine was not released: %v\noutput:\n%s", err, out.String())
	}

	// The cut left the durable record that makes the next field episode
	// attributable: a sync trace naming the hold budget.
	ws, err := workspace.Resolve(root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	entries, err := os.ReadDir(syncTraceDir(ws))
	if err != nil {
		t.Fatalf("read sync trace dir: %v", err)
	}
	// One trace owner per event: the budget explanation must arrive folded
	// into the push attempt's own record (the one carrying the push metadata),
	// never as a second stand-alone record beside it.
	found := false
	for _, entry := range entries {
		content, readErr := os.ReadFile(filepath.Join(syncTraceDir(ws), entry.Name()))
		if readErr != nil {
			continue
		}
		if strings.Contains(string(content), "hold budget") && strings.Contains(string(content), "sync_branch") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no push-attempt sync trace names the hold budget cut; the episode is as unattributable as the field incident")
	}
}
