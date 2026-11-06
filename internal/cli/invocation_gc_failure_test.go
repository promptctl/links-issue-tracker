//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A gc that a git-remote store's close ran, and that failed without being
// cancelled, is one git.gc_failures occurrence on the event of the invocation
// that opened the store, with git's exit status and output, and the mirror's
// marker leaves gc owed again within the hour, not a day on.
//
// Dolt finds git through the process PATH, which no run's Env reaches, so the
// git that fails gc goes on the PATH of a child of this test binary that runs
// only this test; no other test sees it. The shim fails only Dolt's
// `gc --prune=now`, not lit's orphan sweep, which adds --quiet.
func TestAStoreCloseWhoseGCFailedSaysSoOnItsEvent(t *testing.T) {
	if os.Getenv(shimmedChildEnvVar) == "" {
		t.Parallel()
		realGit, err := exec.LookPath("git")
		if err != nil {
			t.Skip("git not available")
		}
		shim := t.TempDir()
		if err := os.WriteFile(filepath.Join(shim, "git"), []byte(`#!/bin/sh
case " $* " in *" gc --prune=now "*) case " $* " in *" --quiet "*) ;; *) echo "gc refused" >&2; exit 3;; esac;; esac
exec '`+strings.ReplaceAll(realGit, `'`, `'\''`)+`' "$@"
`), 0o755); err != nil {
			t.Fatal(err)
		}
		runShimmedChild(t, shim)
		return
	}
	isolateEvents(t)

	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	repo := filepath.Join(base, "alpha")
	runGit(t, base, "clone", filepath.Join(base, "remote.git"), "alpha")
	runGit(t, repo, "config", "user.email", "a@a.co")
	runGit(t, repo, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(repo, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed")
	runGit(t, repo, "push", "origin", "HEAD")
	runCLIInDir(t, repo, "init", "--skip-hooks")
	newEpicInDir(t, repo, "gc-epic")
	runCLIInDir(t, repo, "sync", "push", "--set-upstream")

	ev := lastEvent(t)
	if ev.Event != "lit sync push" {
		t.Fatalf("last event is %q, want the sync push", ev.Event)
	}
	failures := ev.Occurrences["git.gc_failures"]
	if len(failures) == 0 {
		t.Fatalf("git.gc_failures = %v, want the push's store close to have recorded its failed gc", failures)
	}
	for _, failure := range failures {
		// Read it as the events file does: through JSON.
		line, err := json.Marshal(failure)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Mirror   string  `json:"git.gc_failure.mirror"`
			ExitCode int     `json:"git.gc_failure.exit_code"`
			Output   string  `json:"git.gc_failure.output"`
			Error    *string `json:"git.gc_failure.error"`
		}
		if err := json.Unmarshal(line, &got); err != nil {
			t.Fatal(err)
		}
		if got.ExitCode != 3 || got.Output != "gc refused" || got.Error == nil {
			t.Fatalf("occurrence = %s, want exit 3, gc's output and an error", line)
		}
		if info, err := os.Stat(got.Mirror); err != nil || !info.IsDir() {
			t.Fatalf("occurrence = %s, want a mirror that is an existing directory (stat err %v)", line, err)
		}
		// The marker's mtime plus a day is when gc is next owed.
		marker, err := os.Stat(filepath.Join(got.Mirror, ".dolt-gc-last"))
		if err != nil {
			t.Fatalf("mirror %s has no gc marker (stat err %v), want one saying when gc is owed again", got.Mirror, err)
		}
		if owed := marker.ModTime().Add(24 * time.Hour); time.Until(owed) > time.Hour {
			t.Fatalf("mirror %s owes gc again at %v, want within the hour after a gc that failed", got.Mirror, owed)
		}
	}
}
