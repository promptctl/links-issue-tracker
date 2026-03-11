package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCreatesSharedConfigInGitCommonDir(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init")
	info, err := Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if info.WorkspaceID == "" {
		t.Fatal("expected workspace ID")
	}
	if _, err := os.Stat(info.ConfigPath); err != nil {
		t.Fatalf("config file missing: %v", err)
	}
	if _, err := os.Stat(info.StorageDir); err != nil {
		t.Fatalf("storage dir missing: %v", err)
	}
	common := strings.TrimSpace(runOutput(t, repo, "git", "rev-parse", "--git-common-dir"))
	wantStorageDir, err := filepath.EvalSymlinks(filepath.Join(repo, common, "links"))
	if err != nil {
		t.Fatalf("EvalSymlinks(wantStorageDir) error = %v", err)
	}
	gotStorageDir, err := filepath.EvalSymlinks(info.StorageDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(gotStorageDir) error = %v", err)
	}
	if gotStorageDir != wantStorageDir {
		t.Fatalf("storage dir = %q, want %q", info.StorageDir, wantStorageDir)
	}
	info2, err := Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve second call error = %v", err)
	}
	if info2.WorkspaceID != info.WorkspaceID {
		t.Fatalf("workspace ID changed: %q != %q", info2.WorkspaceID, info.WorkspaceID)
	}
}

func TestResolveFailsOutsideGit(t *testing.T) {
	_, err := Resolve(t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
	if err != ErrNotGitRepo {
		t.Fatalf("err = %v, want %v", err, ErrNotGitRepo)
	}
}

func TestGitRemotesReturnsFetchURLsSortedByName(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init")
	run(t, repo, "git", "remote", "add", "upstream", "https://github.com/acme/upstream.git")
	run(t, repo, "git", "remote", "add", "origin", "https://github.com/acme/origin.git")
	run(t, repo, "git", "remote", "set-url", "--push", "origin", "git@github.com:acme/origin.git")

	remotes, err := GitRemotes(repo)
	if err != nil {
		t.Fatalf("GitRemotes() error = %v", err)
	}
	if len(remotes) != 2 {
		t.Fatalf("len(remotes) = %d, want 2", len(remotes))
	}
	if remotes[0].Name != "origin" || remotes[0].URL != "https://github.com/acme/origin.git" {
		t.Fatalf("origin remote mismatch: %+v", remotes[0])
	}
	if remotes[1].Name != "upstream" || remotes[1].URL != "https://github.com/acme/upstream.git" {
		t.Fatalf("upstream remote mismatch: %+v", remotes[1])
	}
}

func TestDefaultRemoteBranchFromSymbolicRef(t *testing.T) {
	branch := defaultRemoteBranchFromSymbolicRef("origin", "origin/main")
	if branch != "main" {
		t.Fatalf("defaultRemoteBranchFromSymbolicRef() = %q, want main", branch)
	}
	if got := defaultRemoteBranchFromSymbolicRef("origin", "upstream/main"); got != "" {
		t.Fatalf("defaultRemoteBranchFromSymbolicRef() = %q, want empty", got)
	}
}

func TestDefaultRemoteBranchFromLSRemote(t *testing.T) {
	output := "ref: refs/heads/main\tHEAD\nc0ffee\tHEAD\n"
	if got := defaultRemoteBranchFromLSRemote(output); got != "main" {
		t.Fatalf("defaultRemoteBranchFromLSRemote() = %q, want main", got)
	}
	if got := defaultRemoteBranchFromLSRemote("c0ffee\trefs/heads/main\n"); got != "" {
		t.Fatalf("defaultRemoteBranchFromLSRemote() = %q, want empty", got)
	}
}

func TestDefaultRemoteBranchUsesRemoteHeadAdvertisement(t *testing.T) {
	repo := t.TempDir()
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, repo, "git", "init")
	run(t, repo, "git", "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) error = %v", err)
	}
	run(t, repo, "git", "add", "README.md")
	run(t, repo, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	run(t, repo, "git", "init", "--bare", remote)
	run(t, repo, "git", "remote", "add", "origin", remote)
	run(t, repo, "git", "push", "-u", "origin", "main")
	run(t, repo, "git", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	got := DefaultRemoteBranch(repo, "origin")
	if got != "main" {
		t.Fatalf("DefaultRemoteBranch() = %q, want main", got)
	}
}

func TestUpstreamRemoteFromRef(t *testing.T) {
	if got := upstreamRemoteFromRef("origin/main"); got != "origin" {
		t.Fatalf("upstreamRemoteFromRef() = %q, want origin", got)
	}
	if got := upstreamRemoteFromRef("upstream/master"); got != "upstream" {
		t.Fatalf("upstreamRemoteFromRef() = %q, want upstream", got)
	}
	if got := upstreamRemoteFromRef("main"); got != "" {
		t.Fatalf("upstreamRemoteFromRef() = %q, want empty", got)
	}
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
}

func runOutput(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}
