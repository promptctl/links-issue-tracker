package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestRemoteCacheKeyMatchesDoltLayout is the pin that makes remoteCacheKey's
// duplication of dbfactory.cacheRepoPath safe to ship. It does a REAL
// git-backed push against a REAL bare remote, lets Dolt create the cache
// directory itself, and asserts our derivation names that exact directory. If
// Dolt ever changes the key — a different hash, a different separator, a
// different default ref — this fails instead of the prune quietly deleting a
// live mirror. [LAW:behavior-not-structure] it asserts the contract our code
// depends on (which directory a remote maps to), never how Dolt computes it.
func TestRemoteCacheKeyMatchesDoltLayout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	remote := seedBareGitRemote(t, base)
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "c1", Topic: "topic", IssueType: "task", Priority: 0,
	}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sync, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer sync.Close()

	gitBacked := GitBackedRemoteURL(remote)
	if !strings.HasPrefix(gitBacked, "git+") {
		t.Fatalf("GitBackedRemoteURL(%q) = %q, want a git+ transport URL", remote, gitBacked)
	}
	if err := sync.SyncAddRemote(ctx, "origin", gitBacked); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := sync.SyncCompactAndPush(ctx, "origin", "master", true, false); err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}

	// What Dolt actually wrote.
	onDisk, err := listRemoteCacheKeys(sync.remoteCacheBase())
	if err != nil {
		t.Fatalf("listRemoteCacheKeys() error = %v", err)
	}
	if len(onDisk) != 1 {
		t.Fatalf("after one git-backed push: cache keys on disk = %v, want exactly 1", onDisk)
	}

	// What we predict, derived from the URL Dolt reports for the remote — the
	// same source pruneRemoteCache reads.
	remotes, err := sync.SyncListRemotes(ctx)
	if err != nil {
		t.Fatalf("SyncListRemotes() error = %v", err)
	}
	if len(remotes) != 1 {
		t.Fatalf("configured remotes = %v, want exactly 1", remotes)
	}
	key, gitBackedRemote, err := remoteCacheKey(remotes[0].URL)
	if err != nil {
		t.Fatalf("remoteCacheKey(%q) error = %v", remotes[0].URL, err)
	}
	if !gitBackedRemote {
		t.Fatalf("remoteCacheKey(%q) reported not git-backed, want git-backed", remotes[0].URL)
	}
	if key != onDisk[0] {
		t.Fatalf("derived cache key = %q, but Dolt created %q for remote url %q.\n"+
			"The derivation in remoteCacheKey no longer matches dbfactory.cacheRepoPath; "+
			"pruning on it would delete live mirrors.", key, onDisk[0], remotes[0].URL)
	}
}

// TestPruneRemoteCacheKeepsTheLiveMirror proves the whole path is safe in the
// ordinary case: one configured remote, one mirror, nothing removed. This is the
// case that runs on every `lit sync push`, so a regression here is a regression
// that churns the entire cache on every push.
func TestPruneRemoteCacheKeepsTheLiveMirror(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	remote := seedBareGitRemote(t, base)
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "c1", Topic: "topic", IssueType: "task", Priority: 0,
	}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sync, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer sync.Close()
	if err := sync.SyncAddRemote(ctx, "origin", GitBackedRemoteURL(remote)); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}

	result, err := sync.SyncCompactAndPush(ctx, "origin", "master", true, false)
	if err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}
	// Nothing was abandoned, so the engine has nothing worth saying and an
	// ordinary push gains no maintenance line at all.
	if result.Maintenance != "" {
		t.Fatalf("Maintenance = %q, want empty on a push with nothing to collect", result.Maintenance)
	}

	live, err := listRemoteCacheKeys(sync.remoteCacheBase())
	if err != nil {
		t.Fatalf("listRemoteCacheKeys() error = %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("after push + prune: cache keys = %v, want the live mirror to survive", live)
	}
}

// TestPruneRemoteCacheCollectsAbandonedMirrors proves the prune does its job:
// a mirror no configured remote maps to is removed, and the live one is not.
func TestPruneRemoteCacheCollectsAbandonedMirrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	remote := seedBareGitRemote(t, base)
	doltRoot := migratedDoltDir(t)

	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "c1", Topic: "topic", IssueType: "task", Priority: 0,
	}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sync, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer sync.Close()
	if err := sync.SyncAddRemote(ctx, "origin", GitBackedRemoteURL(remote)); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := sync.SyncCompactAndPush(ctx, "origin", "master", true, false); err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}

	// Plant an abandoned mirror: a well-formed key directory with real bytes in
	// it that no configured remote derives. This is the shape an org rename or a
	// URL re-spelling leaves behind.
	cacheBase := sync.remoteCacheBase()
	orphan := strings.Repeat("ab", 32)
	orphanDir := filepath.Join(cacheBase, orphan, "repo.git")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("plant orphan error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "packed-refs"), []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatalf("plant orphan payload error = %v", err)
	}

	outcome := sync.pruneRemoteCache(ctx)
	if outcome.Problem != "" {
		t.Fatalf("prune declined unexpectedly: %s", outcome.Problem)
	}
	if outcome.Removed != 1 {
		t.Fatalf("Removed = %d, want 1 (the planted orphan)", outcome.Removed)
	}
	if outcome.Reclaimed < 4096 {
		t.Fatalf("Reclaimed = %d, want at least the orphan's 4096 bytes", outcome.Reclaimed)
	}
	if _, err := os.Stat(filepath.Join(cacheBase, orphan)); !os.IsNotExist(err) {
		t.Fatalf("orphan %s survived the prune (stat err = %v)", orphan, err)
	}

	remaining, err := listRemoteCacheKeys(cacheBase)
	if err != nil {
		t.Fatalf("listRemoteCacheKeys() error = %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("after prune: cache keys = %v, want only the live mirror", remaining)
	}
}

// seedBareGitRemote creates a bare git repo carrying one commit. The commit is
// required, not decoration: dbfactory's ensureRemoteHasBranches refuses to push
// to a remote advertising no heads.
func seedBareGitRemote(t *testing.T, base string) string {
	t.Helper()
	remote := filepath.Join(base, "remote.git")
	gitInDir(t, base, "init", "--bare", "remote.git")

	seed := filepath.Join(base, "seed")
	gitInDir(t, base, "clone", remote, "seed")
	gitInDir(t, seed, "config", "user.email", "seed@example.com")
	gitInDir(t, seed, "config", "user.name", "seed")
	if err := os.WriteFile(filepath.Join(seed, "readme.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed readme error = %v", err)
	}
	gitInDir(t, seed, "add", "-A")
	gitInDir(t, seed, "commit", "-m", "seed")
	gitInDir(t, seed, "push", "origin", "HEAD")
	return remote
}

func gitInDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
}
