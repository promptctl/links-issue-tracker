package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// addLitStore plants the marker Discover keys on — the store database directory
// that store.Open creates and OpenForRead stats — without standing up a real
// Dolt database. Discovery is defined by this path existing, so the fixture only
// needs the path, and the test stays fast and free of store internals.
// [LAW:behavior-not-structure]
func addLitStore(t *testing.T, repoDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git", "links", "dolt"), 0o755); err != nil {
		t.Fatalf("addLitStore(%q) error = %v", repoDir, err)
	}
}

// wantStore computes a repository's canonical StorageDir independently of
// deriveLocation, so the assertion is a genuine oracle rather than the code
// checking itself.
func wantStore(t *testing.T, repoDir string) string {
	t.Helper()
	common, err := filepath.EvalSymlinks(filepath.Join(repoDir, ".git"))
	if err != nil {
		t.Fatalf("EvalSymlinks(%q/.git) error = %v", repoDir, err)
	}
	return filepath.Join(common, "links")
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	return path
}

// TestDiscoverListsExactlyTheDistinctStores is the ticket criterion: a tree of
// known lit repos, plus non-lit directories, plus a multi-worktree repo, must
// yield exactly the distinct store locations — no misses, no duplicates, no
// non-lit directories.
func TestDiscoverListsExactlyTheDistinctStores(t *testing.T) {
	root := t.TempDir()

	repoA := mkdir(t, filepath.Join(root, "repoA"))
	run(t, repoA, "git", "init")
	addLitStore(t, repoA)

	repoB := mkdir(t, filepath.Join(root, "repoB"))
	run(t, repoB, "git", "init")
	addLitStore(t, repoB)

	// A multi-worktree lit repo: two working trees under the scanned root, one
	// shared store. Discovery must collapse them to a single location.
	wtMain := mkdir(t, filepath.Join(root, "wtMain"))
	run(t, wtMain, "git", "init")
	run(t, wtMain, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	addLitStore(t, wtMain)
	wtLinked := filepath.Join(root, "wtLinked")
	run(t, wtMain, "git", "worktree", "add", wtLinked)

	// A git repository that was never `lit init`ed — must be skipped.
	gitOnly := mkdir(t, filepath.Join(root, "gitOnly"))
	run(t, gitOnly, "git", "init")

	// A non-git directory (with a nested non-git subdir) — must be skipped.
	mkdir(t, filepath.Join(root, "plain", "nested"))

	locations, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	got := make([]string, 0, len(locations))
	for _, loc := range locations {
		got = append(got, loc.StorageDir)
	}
	// Discover returns StorageDir-sorted; the wants are listed in that order.
	want := []string{wantStore(t, repoA), wantStore(t, repoB), wantStore(t, wtMain)}

	if len(got) != len(want) {
		t.Fatalf("Discover() returned %d stores, want %d\n got=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("store[%d] = %q, want %q\n got=%v\nwant=%v", i, got[i], want[i], got, want)
		}
	}
}

// TestDiscoverReturnsNoStoresWhenNoneExist confirms a tree of only non-lit
// directories and lit-less git repos yields an empty result, not a false hit.
func TestDiscoverReturnsNoStoresWhenNoneExist(t *testing.T) {
	root := t.TempDir()
	gitOnly := mkdir(t, filepath.Join(root, "gitOnly"))
	run(t, gitOnly, "git", "init")
	mkdir(t, filepath.Join(root, "plain", "deeper"))

	locations, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(locations) != 0 {
		t.Fatalf("Discover() = %v, want no stores", locations)
	}
}

// TestDiscoverMatchesResolveDerivation locks the discovery/resolve seam: the
// location Discover reports for a repo is byte-identical to the store Resolve
// opens there. If the two derivations ever drift, discovery would list a store
// the tracker doesn't actually use.
func TestDiscoverMatchesResolveDerivation(t *testing.T) {
	root := t.TempDir()
	repo := mkdir(t, filepath.Join(root, "repo"))
	run(t, repo, "git", "init")

	info, err := Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	// Resolve created the store directory; a store database still has to exist for
	// discovery to count it, mirroring a real `lit init`.
	addLitStore(t, repo)

	locations, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(locations) != 1 {
		t.Fatalf("Discover() = %v, want exactly the one resolved store", locations)
	}
	if locations[0].StorageDir != info.StorageDir {
		t.Fatalf("Discover StorageDir = %q, Resolve StorageDir = %q", locations[0].StorageDir, info.StorageDir)
	}
	if locations[0].DatabasePath != info.DatabasePath {
		t.Fatalf("Discover DatabasePath = %q, Resolve DatabasePath = %q", locations[0].DatabasePath, info.DatabasePath)
	}
}
