//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/version"
)

// The real entrypoint reads the Origin stamp into the run's Env, and that one
// line is what makes a shipped source build refuse to raise a remote: every
// package-level test Env is a release, so only the binary itself can show
// the wiring. A source-stamped lit's first push to a remote with no head on
// the branch is a raise from nothing and is refused; the same push from a
// release-stamped lit lands. [LAW:behavior-not-structure]
func TestTheEntrypointReadsTheRaisePolicyFromTheOriginStamp(t *testing.T) {
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
	xdgConfigHome := filepath.Join(base, "xdg-config")
	if err := os.Mkdir(xdgConfigHome, 0o755); err != nil {
		t.Fatalf("mkdir xdg config home: %v", err)
	}
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "origin@test.co")
	runGit(t, root, "config", "user.name", "origin")
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

	// Auto-sync stays off: the push under test is the one explicit push.
	env := isolatedEnv(xdgConfigHome, "1")
	if out, err := runLit(t, root, self, env, "init", "--skip-hooks"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out)
	}

	asSource := map[string]string{reexecOriginEnvVar: version.OriginSource}
	for k, v := range env {
		asSource[k] = v
	}
	out, err := runLit(t, root, self, asSource, "sync", "push", "--set-upstream")
	if err == nil {
		t.Fatalf("a source build's first push to a remote with no head landed:\n%s", out)
	}
	if !strings.Contains(out, "would raise the remote's schema and this lit is not a release") {
		t.Fatalf("a source build's first push was refused for something other than the raise:\n%s", out)
	}

	if out, err := runLit(t, root, self, env, "sync", "push", "--set-upstream"); err != nil {
		t.Fatalf("a release's first push to a remote with no head was refused: %v\noutput:\n%s", err, out)
	}
}
