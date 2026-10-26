//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestEachCommandAsksGitEachQuestionOnce pins, for the commands an agent runs
// most, the exact git each one starts: one subprocess per question the command
// asks, however many worktrees the repository has and however many parts of the
// command want the same answer. The repository has a linked worktree and a
// remote its branch tracks, so the per-worktree and per-remote questions are all
// live — save the upstream, which no command asks with a single remote, the one
// remote being the only answer it could give. Only the git the command's own process starts is counted — a detached
// receive or mirror worker is its own invocation, with its own event.
//
// Each command's event carries the same count, as git.lit_spawns, so a
// regression is read off the events file rather than reconstructed.
func TestEachCommandAsksGitEachQuestionOnce(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}
	base := t.TempDir()
	root := filepath.Join(base, "primary")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	xdgConfigHome := filepath.Join(base, "xdg-config")
	stateHome := filepath.Join(base, "xdg-state")
	for _, dir := range []string{xdgConfigHome, stateHome} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "spawns@test.co")
	runGit(t, root, "config", "user.name", "spawns")
	runGit(t, root, "commit", "--allow-empty", "-m", "seed")
	branch := gitCurrentBranch(t, root)
	runGit(t, base, "init", "--bare", "remote.git")
	runGit(t, root, "remote", "add", "origin", filepath.Join(base, "remote.git"))
	runGit(t, root, "push", "-u", "origin", branch)
	runGit(t, root, "worktree", "add", "-b", "linked", filepath.Join(base, "linked"))

	quiet := isolatedEnv(xdgConfigHome, "1")
	if out, err := runLit(t, root, self, quiet, "init", "--skip-hooks"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out)
	}
	epic := newEpicViaLit(t, root, self, quiet, "spawns-epic")
	// `lit next` reports an empty backlog as a failure; a ready task is the
	// ordinary state it routes from.
	if out, err := runLit(t, root, self, quiet, "new", "--type", "task", "--title", "spawns-ready", "--topic", "demo", "--parent", epic); err != nil {
		t.Fatalf("lit new task: %v\noutput:\n%s", err, out)
	}

	log := filepath.Join(base, "git.log")
	shimDir := filepath.Join(base, "git-shim")
	if err := os.Mkdir(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\t%%s\\n' \"$PPID\" \"$*\" >> %q\nexec %q \"$@\"\n", log, realGit)
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	measured := isolatedEnv(xdgConfigHome, "0")
	measured["PATH"] = shimDir + string(os.PathListSeparator) + os.Getenv("PATH")
	measured["XDG_STATE_HOME"] = stateHome

	geometry := "rev-parse --show-toplevel --git-common-dir --git-dir"
	// The remote-HEAD question is the repository's, not the checkout's: it
	// names the git dir explicitly, as git itself reports it (symlinks
	// resolved, so /tmp's macOS spelling does not leak into the want).
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(resolvedRoot, ".git")
	remoteHead := "--git-dir " + gitDir + " --work-tree " + gitDir + " symbolic-ref --quiet refs/remotes/origin/HEAD"
	// The remote list is the repository's too, asked the same way.
	remoteList := "--git-dir " + gitDir + " --work-tree " + gitDir + " remote -v"
	freshness := []string{geometry, remoteList, remoteHead, "worktree list --porcelain -z"}
	// Order matters only for the receive: the first command that wants one
	// asks whether there is a remote to receive from, and the debounce keeps
	// the commands after it from scheduling another.
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"quickstart"}, []string{geometry}},
		{[]string{"workspace"}, []string{geometry}},
		{[]string{"ls"}, []string{geometry, remoteList}},
		{[]string{"backlog"}, freshness},
		{[]string{"next"}, freshness},
		{[]string{"new", "--type", "epic", "--title", "spawns-write", "--topic", "demo"}, []string{geometry, remoteList}},
	} {
		if err := os.WriteFile(log, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		pid, out := startLit(t, root, self, measured, tc.args...)
		if got := gitStartedBy(t, log, pid); !slices.Equal(got, tc.want) {
			t.Errorf("lit %s started git:\n  %s\nwant:\n  %s\noutput:\n%s", tc.args[0], strings.Join(got, "\n  "), strings.Join(tc.want, "\n  "), out)
		}
		if got := eventGitSpawns(t, stateHome, pid); got != len(tc.want) {
			t.Errorf("lit %s event git.lit_spawns = %d, want %d", tc.args[0], got, len(tc.want))
		}
	}
}

// startLit runs lit to completion and returns its pid, which is the parent
// pid of every git it started itself.
func startLit(t *testing.T, dir, self string, extraEnv map[string]string, args ...string) (int, string) {
	t.Helper()
	joinDetachedWorkers(t, dir)
	cmd := exec.Command(self, args...)
	cmd.Dir = dir
	cmd.Env = litEnv(extraEnv)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("lit %s: %v\noutput:\n%s", strings.Join(args, " "), err, out.String())
	}
	return cmd.Process.Pid, out.String()
}

// gitStartedBy is the git argv, in order, the shim logged with pid as parent,
// without the -c core.longpaths=true every lit git carries.
func gitStartedBy(t *testing.T, log string, pid int) []string {
	t.Helper()
	content, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var argvs []string
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		parent, argv, ok := strings.Cut(line, "\t")
		if ok && parent == strconv.Itoa(pid) {
			argvs = append(argvs, strings.TrimPrefix(argv, "-c core.longpaths=true "))
		}
	}
	return argvs
}

// eventGitSpawns is git.lit_spawns from the event the lit at pid wrote.
func eventGitSpawns(t *testing.T, stateHome string, pid int) int {
	t.Helper()
	file, err := os.Open(filepath.Join(stateHome, "links-issue-tracker", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var ev struct {
			PID    int            `json:"pid"`
			Counts map[string]int `json:"counts"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if ev.PID != pid {
			continue
		}
		count, ok := ev.Counts["git.lit_spawns"]
		if !ok {
			t.Fatalf("the event of pid %d has no git.lit_spawns count: %v", pid, ev.Counts)
		}
		return count
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("no event from pid %d", pid)
	return 0
}
