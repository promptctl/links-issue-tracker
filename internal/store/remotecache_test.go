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

// TestPruneRemoteCacheIsNotBlockedByOneStuckMirror pins the head-of-line
// property. plan.abandoned is sorted, so an entry that can never be removed is
// reached first on every push for the life of the workspace; returning on it
// meant nothing sorting after it was ever attempted again, however removable.
// The failure still has to be reported — the point is that it stops being a
// gate on everyone else's collection.
func TestPruneRemoteCacheIsNotBlockedByOneStuckMirror(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not block unlink")
	}
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

	// Two orphans, and the stuck one sorts first so it is reached first. Its own
	// directory is read-only, so the walk that measures it succeeds while the
	// unlink of what is inside it cannot.
	cacheBase := sync.remoteCacheBase()
	stuck, collectible := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	for _, key := range []string{stuck, collectible} {
		repo := filepath.Join(cacheBase, key, "repo.git")
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatalf("plant orphan %s: %v", key, err)
		}
		if err := os.WriteFile(filepath.Join(repo, "packed-refs"), make([]byte, 4096), 0o644); err != nil {
			t.Fatalf("plant orphan payload %s: %v", key, err)
		}
	}
	if err := os.Chmod(filepath.Join(cacheBase, stuck), 0o500); err != nil {
		t.Fatalf("chmod stuck orphan: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(cacheBase, stuck), 0o700) })

	outcome := sync.pruneRemoteCache(ctx)

	if outcome.Removed != 1 {
		t.Fatalf("Removed = %d, want 1 — the collectible orphan sorts after the stuck one "+
			"and must still be collected (Problem = %q)", outcome.Removed, outcome.Problem)
	}
	if _, err := os.Stat(filepath.Join(cacheBase, collectible)); !os.IsNotExist(err) {
		t.Fatalf("the collectible orphan survived: one stuck mirror is blocking the rest (stat err = %v)", err)
	}
	if !strings.Contains(outcome.Problem, stuck) {
		t.Fatalf("Problem = %q, want the stuck mirror still reported by key", outcome.Problem)
	}
	if _, err := os.Stat(filepath.Join(cacheBase, stuck)); err != nil {
		t.Fatalf("the stuck orphan should still be there to report on: %v", err)
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

// TestRemoteCacheKeyIsStillAbandonedFollowsTheRemotesNotASnapshot pins the
// re-check the delete loop leans on. The plan answers "is this abandoned?" once,
// for every key, at a moment that recedes as the loop runs; this asks it again of
// the authority in the moment before a directory is removed. The third case is
// the one that matters: the same key changes answer when the remote list
// changes, which is exactly what a snapshot cannot do.
func TestRemoteCacheKeyIsStillAbandonedFollowsTheRemotesNotASnapshot(t *testing.T) {
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

	keys, err := listRemoteCacheKeys(sync.remoteCacheBase())
	if err != nil {
		t.Fatalf("listRemoteCacheKeys() error = %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("cache keys after push = %v, want exactly the live mirror", keys)
	}
	mirrorKey := keys[0]

	// A key origin derives is not abandoned, whatever a stale plan believed.
	if abandoned, err := sync.remoteCacheKeyIsStillAbandoned(ctx, mirrorKey); err != nil || abandoned {
		t.Fatalf("remoteCacheKeyIsStillAbandoned(live) = (%v, %v), want (false, nil)", abandoned, err)
	}
	// A key no remote derives is.
	if abandoned, err := sync.remoteCacheKeyIsStillAbandoned(ctx, strings.Repeat("ab", 32)); err != nil || !abandoned {
		t.Fatalf("remoteCacheKeyIsStillAbandoned(orphan) = (%v, %v), want (true, nil)", abandoned, err)
	}
	// Drop the remote and the same key changes answer. A plan taken before this
	// point still calls it live; the re-check does not, because it reads the
	// remote list rather than remembering it.
	if err := sync.SyncRemoveRemote(ctx, "origin"); err != nil {
		t.Fatalf("SyncRemoveRemote() error = %v", err)
	}
	if abandoned, err := sync.remoteCacheKeyIsStillAbandoned(ctx, mirrorKey); err != nil || !abandoned {
		t.Fatalf("after removing origin: remoteCacheKeyIsStillAbandoned(%s) = (%v, %v), want (true, nil) — "+
			"the answer must follow the remotes, not a snapshot", mirrorKey, abandoned, err)
	}
}
