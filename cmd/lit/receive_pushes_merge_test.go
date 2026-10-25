//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestAReceiveThatMergesARefusedPushPushesTheMerge is links-storage-3k6u.bzg's
// acceptance pin, on the shipped default config: alpha and bravo edit
// different fields of one ticket without syncing, alpha's push lands, and the
// remote refuses bravo's. bravo then runs one read command. Its automatic
// receive merges alpha's edit, and the merge reaches the remote with no
// further write from bravo: the receive owes it the on-change mirror a write
// is owed.
//
// The oracle is a remoteObserver, which sees only what the remote holds. A
// read command spawns no mirror and nothing else in the test pushes, so if
// the receive owed nothing the poll exhausts its deadline. The deadline
// covers the whole chain: the command's exit, the receive's fetch and
// reconcile, the worker's exit, then the mirror's push.
func TestAReceiveThatMergesARefusedPushPushesTheMerge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}
	base := t.TempDir()
	xdgConfigHome := filepath.Join(base, "xdg-config")
	if err := os.Mkdir(xdgConfigHome, 0o755); err != nil {
		t.Fatalf("mkdir xdg config home: %v", err)
	}
	quiet := isolatedEnv(xdgConfigHome, "1")
	mustLit := func(dir string, args ...string) string {
		t.Helper()
		out, err := runLit(t, dir, self, quiet, args...)
		if err != nil {
			t.Fatalf("lit %s in %s: %v\n%s", strings.Join(args, " "), filepath.Base(dir), err, out)
		}
		return out
	}

	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")
	alpha := filepath.Join(base, "alpha")
	runGit(t, base, "clone", remote, "alpha")
	runGit(t, alpha, "config", "user.email", "a@a.co")
	runGit(t, alpha, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(alpha, "readme.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, alpha, "add", "-A")
	runGit(t, alpha, "commit", "-m", "seed")
	runGit(t, alpha, "push", "origin", "HEAD")
	mustLit(alpha, "init", "--skip-hooks")
	epic := newEpicViaLit(t, alpha, self, quiet, "shared-epic")
	ticket := issueIDFromLitNew(t, mustLit(alpha, "new", "--title", "shared-ticket", "--topic", "demo", "--type", "task", "--parent", epic), "shared-ticket")
	mustLit(alpha, "sync", "push", "--set-upstream")

	bravo := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, bravo, "config", "user.email", "b@b.co")
	runGit(t, bravo, "config", "user.name", "bravo")
	mustLit(bravo, "init", "--skip-hooks")
	observer := newRemoteObserver(t, base, remote, self, xdgConfigHome)

	mustLit(alpha, "update", ticket, "--description", "alpha-description")
	mustLit(alpha, "sync", "push")
	mustLit(bravo, "update", ticket, "--title", "bravo-title")
	if out, err := runLit(t, bravo, self, quiet, "sync", "push"); err == nil {
		t.Fatalf("bravo's push over alpha's landed; want the remote to refuse it:\n%s", out)
	}
	// The refusal is classified by bravo's config, which runs the receive,
	// not by the kill switch the push ran under.
	bravoWS, err := workspace.Resolve(testEnv(), bravo)
	if err != nil {
		t.Fatalf("resolve bravo: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(bravoWS.StorageDir, "push-outcome.last"))
	if err != nil {
		t.Fatalf("read bravo's push-outcome marker: %v", err)
	}
	var refused struct {
		Decision      string `json:"decision"`
		ReceiveMerges bool   `json:"receive_merges"`
	}
	if err := json.Unmarshal(marker, &refused); err != nil || refused.Decision != "diverged" || !refused.ReceiveMerges {
		t.Fatalf("bravo's push-outcome marker = %s (err %v); want diverged with receive_merges", marker, err)
	}

	if out, err := runLit(t, bravo, self, isolatedEnv(xdgConfigHome, "0"), "backlog"); err != nil {
		t.Fatalf("bravo's lit backlog: %v\n%s", err, out)
	}

	const pollTimeout = 90 * time.Second
	const pollInterval = 500 * time.Millisecond
	var arrived int
	var lastErr error
	for deadline := time.Now().Add(pollTimeout); time.Now().Before(deadline); time.Sleep(pollInterval) {
		if arrived, lastErr = observer.count(epic, "bravo-title"); lastErr == nil && arrived == 1 {
			return
		}
	}
	t.Fatalf("bravo's merge never reached the remote %s after its receive was due; last observation: %d arrived, error %v", pollTimeout, arrived, lastErr)
}
