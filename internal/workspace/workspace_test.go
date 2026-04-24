package workspace

import (
	"encoding/json"
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
	if info.IssuePrefix == "" {
		t.Fatal("expected issue prefix")
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
	if info2.IssuePrefix != info.IssuePrefix {
		t.Fatalf("issue prefix changed: %q != %q", info2.IssuePrefix, info.IssuePrefix)
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

func TestResolveNormalizesConfiguredIssuePrefix(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init")

	info, err := Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() initial error = %v", err)
	}

	payload, err := os.ReadFile(info.ConfigPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(payload, &cfg); err != nil {
		t.Fatalf("json.Unmarshal(config) error = %v", err)
	}
	cfg.IssuePrefix = "Renderer Platform Team"
	updated, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal(config) error = %v", err)
	}
	if err := os.WriteFile(info.ConfigPath, updated, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	info, err = Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() normalized error = %v", err)
	}
	if info.IssuePrefix != "renderer-pla" {
		t.Fatalf("IssuePrefix = %q, want renderer-pla", info.IssuePrefix)
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
	branch := defaultRemoteBranchFromSymbolicRef("origin", "origin/master")
	if branch != "master" {
		t.Fatalf("defaultRemoteBranchFromSymbolicRef() = %q, want master", branch)
	}
	if got := defaultRemoteBranchFromSymbolicRef("origin", "upstream/master"); got != "" {
		t.Fatalf("defaultRemoteBranchFromSymbolicRef() = %q, want empty", got)
	}
}

func TestDefaultRemoteBranchFromLSRemote(t *testing.T) {
	output := "ref: refs/heads/master\tHEAD\nc0ffee\tHEAD\n"
	if got := defaultRemoteBranchFromLSRemote(output); got != "master" {
		t.Fatalf("defaultRemoteBranchFromLSRemote() = %q, want master", got)
	}
	if got := defaultRemoteBranchFromLSRemote("c0ffee\trefs/heads/master\n"); got != "" {
		t.Fatalf("defaultRemoteBranchFromLSRemote() = %q, want empty", got)
	}
}

func TestDefaultRemoteBranchUsesRemoteHeadAdvertisement(t *testing.T) {
	repo := t.TempDir()
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, repo, "git", "init")
	run(t, repo, "git", "checkout", "-b", "master")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) error = %v", err)
	}
	run(t, repo, "git", "add", "README.md")
	run(t, repo, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	run(t, repo, "git", "init", "--bare", remote)
	run(t, repo, "git", "remote", "add", "origin", remote)
	run(t, repo, "git", "push", "-u", "origin", "master")
	run(t, repo, "git", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/master")

	got := DefaultRemoteBranch(repo, "origin")
	if got != "master" {
		t.Fatalf("DefaultRemoteBranch() = %q, want master", got)
	}
}

func TestRemoteHasRefsReturnsFalseForEmptyBareRemote(t *testing.T) {
	repo := t.TempDir()
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, repo, "git", "init")
	run(t, repo, "git", "init", "--bare", remote)
	run(t, repo, "git", "remote", "add", "origin", remote)

	hasRefs, err := RemoteHasRefs(repo, "origin")
	if err != nil {
		t.Fatalf("RemoteHasRefs() error = %v, want nil for empty bare remote", err)
	}
	if hasRefs {
		t.Fatalf("RemoteHasRefs() = true, want false for empty bare remote")
	}
}

func TestRemoteHasRefsReturnsTrueForPopulatedRemote(t *testing.T) {
	repo := t.TempDir()
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, repo, "git", "init")
	run(t, repo, "git", "checkout", "-b", "master")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) error = %v", err)
	}
	run(t, repo, "git", "add", "README.md")
	run(t, repo, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	run(t, repo, "git", "init", "--bare", remote)
	run(t, repo, "git", "remote", "add", "origin", remote)
	run(t, repo, "git", "push", "-u", "origin", "master")

	hasRefs, err := RemoteHasRefs(repo, "origin")
	if err != nil {
		t.Fatalf("RemoteHasRefs() error = %v, want nil for populated remote", err)
	}
	if !hasRefs {
		t.Fatalf("RemoteHasRefs() = false, want true for populated remote")
	}
}

func TestRemoteHasRefsReturnsErrorForUnknownRemoteName(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init")

	hasRefs, err := RemoteHasRefs(repo, "does-not-exist")
	if err == nil {
		t.Fatalf("RemoteHasRefs() error = nil, want error for unknown remote (got hasRefs=%v)", hasRefs)
	}
	if hasRefs {
		t.Fatalf("RemoteHasRefs() = true, want false alongside error")
	}
}

func TestUpstreamRemoteFromRef(t *testing.T) {
	if got := upstreamRemoteFromRef("origin/master"); got != "origin" {
		t.Fatalf("upstreamRemoteFromRef() = %q, want origin", got)
	}
	if got := upstreamRemoteFromRef("upstream/master"); got != "upstream" {
		t.Fatalf("upstreamRemoteFromRef() = %q, want upstream", got)
	}
	if got := upstreamRemoteFromRef("master"); got != "" {
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
