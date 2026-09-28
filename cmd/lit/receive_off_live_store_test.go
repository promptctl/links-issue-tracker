package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReceiveFetchLeavesTheLiveStoreFree pins that the automatic receive's
// fetch runs on a clone, so a write that lands on the live store while the
// fetch is on the network goes straight through rather than waiting out
// coResidentHolderWait and failing with the receiving command named.
//
// The network round trip is made long and observable with a git shim on the
// receiving command's PATH: Dolt's blobstore fetch is the one git call that
// passes `--refmap=`, and the shim marks its start and sleeps before running
// the real git. The write runs with the ordinary PATH while that sleep is in
// progress. The consumer then has a local commit the remote lacks while the
// remote has one it lacks, so the receive also exercises the settle's
// divergence arm: after it, the consumer holds both tickets.
func TestReceiveFetchLeavesTheLiveStoreFree(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
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

	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")
	producer := filepath.Join(base, "alpha")
	runGit(t, base, "clone", remote, "alpha")
	runGit(t, producer, "config", "user.email", "a@a.co")
	runGit(t, producer, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(producer, "readme.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, producer, "add", "-A")
	runGit(t, producer, "commit", "-m", "seed")
	runGit(t, producer, "push", "origin", "HEAD")
	mustLit := func(dir string, env map[string]string, args ...string) string {
		t.Helper()
		out, err := runLit(t, dir, self, env, args...)
		if err != nil {
			t.Fatalf("lit %s in %s: %v\n%s", strings.Join(args, " "), filepath.Base(dir), err, out)
		}
		return out
	}
	mustLit(producer, quiet, "init", "--skip-hooks", "--skip-agents")
	mustLit(producer, quiet, "new", "--title", "first-ticket", "--topic", "demo", "--type", "task")
	mustLit(producer, quiet, "sync", "push", "--set-upstream")

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	mustLit(consumer, quiet, "init", "--skip-hooks", "--skip-agents")

	mustLit(producer, quiet, "new", "--title", "peer-ticket", "--topic", "demo", "--type", "task")
	mustLit(producer, quiet, "sync", "push")

	const fetchStall = 5 * time.Second
	shimDir := filepath.Join(base, "shim")
	if err := os.Mkdir(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim dir: %v", err)
	}
	started := filepath.Join(base, "fetch-started")
	shim := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = \"--refmap=\" ]; then touch %q; sleep %d; fi\ndone\nexec %q \"$@\"\n",
		started, int(fetchStall/time.Second), realGit)
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}

	receiving := isolatedEnv(xdgConfigHome, "0")
	receiving["PATH"] = shimDir + string(os.PathListSeparator) + os.Getenv("PATH")
	receiver := exec.Command(self, "backlog")
	receiver.Dir = consumer
	receiver.Env = litEnv(receiving)
	var receiverOut bytes.Buffer
	receiver.Stdout, receiver.Stderr = &receiverOut, &receiverOut
	if err := receiver.Start(); err != nil {
		t.Fatalf("start the receiving command: %v", err)
	}
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Wait() }()

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-receiverDone:
			t.Fatalf("the receiving command exited (%v) before its fetch started:\n%s", err, receiverOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the receive's fetch never started:\n%s", receiverOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	writeStart := time.Now()
	out, err := runLit(t, consumer, self, quiet, "new", "--title", "local-ticket", "--topic", "demo", "--type", "task")
	writeTook := time.Since(writeStart)
	if err != nil {
		t.Fatalf("a write during the receive's fetch failed after %s; the fetch must not hold the live store:\n%s", writeTook, out)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("stat fetch marker: %v", err)
	}
	select {
	case err := <-receiverDone:
		t.Fatalf("the receiving command finished (%v) before the write did, so the write did not overlap the fetch; the stall is too short to prove anything", err)
	default:
	}

	select {
	case err := <-receiverDone:
		if err != nil {
			t.Fatalf("the receiving command failed: %v\n%s", err, receiverOut.String())
		}
	case <-time.After(fetchStall + 60*time.Second):
		t.Fatalf("the receiving command did not finish:\n%s", receiverOut.String())
	}
	// A reconcile that committed may have spawned a mirror; it must not race
	// the TempDir sweep.
	awaitMirrorQuiescence(t, consumer)
	backlog := mustLit(consumer, quiet, "backlog")
	for _, title := range []string{"first-ticket", "peer-ticket", "local-ticket"} {
		if !strings.Contains(backlog, title) {
			t.Fatalf("consumer backlog after the receive lacks %q:\n%s\nreceiving command output:\n%s", title, backlog, receiverOut.String())
		}
	}
	if entries, err := os.ReadDir(filepath.Join(consumer, ".git", "links", "receive-clone")); err == nil && len(entries) != 0 {
		t.Fatalf("the receive left %d clone(s) behind under receive-clone", len(entries))
	}
}
