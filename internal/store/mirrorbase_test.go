package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gitDirLimit is the longest GIT_DIR Git for Windows accepts: PATH_MAX-40.
const gitDirLimit = 260 - 40

// A store's mirror base, and so every GIT_DIR git is handed for its mirrors,
// is as long at any repository depth: from a deep repository a mirror inside
// it would pass git's limit.
func TestMirrorBaseLengthDoesNotGrowWithTheRepositorysDepth(t *testing.T) {
	t.Parallel()
	mirrorRoot := filepath.Join(t.TempDir(), "m")
	shallow := MirrorBase(mirrorRoot, filepath.Join(t.TempDir(), "dolt"))
	deep := MirrorBase(mirrorRoot, filepath.Join(t.TempDir(), strings.Repeat("d", 200), strings.Repeat("e", 200), "dolt"))
	if len(shallow) != len(deep) {
		t.Fatalf("mirror base of a shallow store is %d characters, of a deep one %d; want the same", len(shallow), len(deep))
	}
	if filepath.Dir(deep) != mirrorRoot {
		t.Fatalf("mirror base %s is not directly under the mirror root %s", deep, mirrorRoot)
	}
}

// A store at a path past git's GIT_DIR limit on Windows pushes and fetches
// through a mirror that lives under the mirror root, at a GIT_DIR git accepts.
func TestASyncFromADeepRepositoryKeepsItsMirrorUnderTheMirrorRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	remote := seedBareGitRemote(t, base)

	deepRoot := filepath.Join(t.TempDir(), strings.Repeat("d", 150), strings.Repeat("e", 150), "dolt")
	if err := os.MkdirAll(filepath.Dir(deepRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(migratedDoltDir(t), deepRoot); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, testEnv(), testStderr(), deepRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	commitPayload(t, ctx, st, "c1")
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	sync, err := OpenSync(ctx, testEnv(), testStderr(), deepRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer sync.Close()
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), GitBackedRemoteURL(remote)); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := sync.SyncCompactAndPush(ctx, ctx, remoteName(t, "origin"), "master", unfiredBound(t), true, false); err != nil {
		t.Fatalf("SyncCompactAndPush() error = %v", err)
	}
	if err := sync.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch() error = %v", err)
	}

	keys, err := listRemoteCacheKeys(sync.remoteCacheBase())
	if err != nil || len(keys) != 1 {
		t.Fatalf("mirrors under %s = %v (%v), want the one remote's", sync.remoteCacheBase(), keys, err)
	}
	if !isUnder(sync.remoteCacheBase(), testMirrorRoot) {
		t.Fatalf("mirror base %s is not under the mirror root %s", sync.remoteCacheBase(), testMirrorRoot)
	}
	// The GIT_DIR the mirror would carry on a Windows runner, whose mirror
	// root is %LocalAppData%\lit\m: the same base name and key under it.
	windowsGitDir := filepath.Join(`C:\Users\runneradmin\AppData\Local\lit\m`, filepath.Base(sync.remoteCacheBase()), keys[0], "repo.git")
	if len(windowsGitDir) > gitDirLimit {
		t.Fatalf("GIT_DIR %s is %d characters, past git's %d", windowsGitDir, len(windowsGitDir), gitDirLimit)
	}
	if _, err := os.Stat(legacyRemoteCacheBase(deepRoot)); !os.IsNotExist(err) {
		t.Fatalf("an in-repository cache exists at %s (%v); want none", legacyRemoteCacheBase(deepRoot), err)
	}
}

// writeBase lays down a mirror base under mirrorRoot for the store rooted at
// doltRoot, holding one file, as an open would leave it.
func writeBase(t *testing.T, mirrorRoot, doltRoot string) string {
	t.Helper()
	base, err := ensureMirrorBase(mirrorRoot, doltRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, strings.Repeat("a", 64), "repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, strings.Repeat("a", 64), "repo.git", "HEAD"), []byte("ref: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base
}

// The prune removes the base of a repository that was deleted, and keeps the
// base of one that stands, including one whose database an adopt has set
// aside while it clones a new one into place.
func TestThePruneSweepsTheMirrorBaseOfADeletedRepositoryOnly(t *testing.T) {
	t.Parallel()
	mirrorRoot := t.TempDir()
	deletedRepo := filepath.Join(t.TempDir(), "gone")
	liveRoot := filepath.Join(t.TempDir(), "dolt")
	if err := os.MkdirAll(liveRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gone := writeBase(t, mirrorRoot, filepath.Join(deletedRepo, "dolt"))
	live := writeBase(t, mirrorRoot, liveRoot)

	outcome := sweepOrphanedMirrorBases(mirrorRoot)
	if outcome.Problem != "" || outcome.Removed != 1 {
		t.Fatalf("sweep = %+v, want one base removed and no problem", outcome)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("the deleted repository's base %s still exists (%v)", gone, err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the live repository's base %s is gone: %v", live, err)
	}
}

// A take's sweep removes the mirror bases of every clone a killed cycle left
// under its clone base, and no other base.
func TestATakesSweepRemovesTheMirrorBasesOfDeadClonesUnderItsBase(t *testing.T) {
	t.Parallel()
	mirrorRoot := t.TempDir()
	storage := t.TempDir()
	cloneBase := filepath.Join(storage, "mirror-clone")
	deadClone := writeBase(t, mirrorRoot, filepath.Join(cloneBase, "123", "dolt"))
	otherClone := writeBase(t, mirrorRoot, filepath.Join(storage, "receive-clone", "456", "dolt"))
	live := writeBase(t, mirrorRoot, filepath.Join(storage, "dolt"))

	if err := SweepMirrorBasesUnder(mirrorRoot, cloneBase); err != nil {
		t.Fatalf("SweepMirrorBasesUnder() error = %v", err)
	}
	if _, err := os.Stat(deadClone); !os.IsNotExist(err) {
		t.Fatalf("the dead clone's base %s still exists (%v)", deadClone, err)
	}
	for _, kept := range []string{otherClone, live} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("base %s outside the swept clone base is gone: %v", kept, err)
		}
	}
}

// A clone starts with a copy of the live store's mirrors, recorded as its
// own, and its removal leaves the live mirrors in place.
func TestACloneIsSeededWithTheLiveMirrorsAndRemovesOnlyItsOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mirrorRoot := t.TempDir()
	liveRoot := filepath.Join(t.TempDir(), "dolt")
	cloneRoot := filepath.Join(t.TempDir(), "clone", "dolt")
	live := writeBase(t, mirrorRoot, liveRoot)

	if err := SeedCloneMirrors(ctx, mirrorRoot, liveRoot, cloneRoot); err != nil {
		t.Fatalf("SeedCloneMirrors() error = %v", err)
	}
	clone := MirrorBase(mirrorRoot, cloneRoot)
	if _, err := os.Stat(filepath.Join(clone, strings.Repeat("a", 64), "repo.git", "HEAD")); err != nil {
		t.Fatalf("the clone's base lacks the live mirror: %v", err)
	}
	recorded, err := os.ReadFile(filepath.Join(clone, rootFileName))
	if err != nil || string(recorded) != mirrorBaseIdentity(cloneRoot) {
		t.Fatalf("the clone's base records %q (%v), want %q", recorded, err, mirrorBaseIdentity(cloneRoot))
	}

	if err := RemoveCloneMirrors(mirrorRoot, cloneRoot); err != nil {
		t.Fatalf("RemoveCloneMirrors() error = %v", err)
	}
	if _, err := os.Stat(clone); !os.IsNotExist(err) {
		t.Fatalf("the clone's base %s still exists (%v)", clone, err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the live base %s is gone: %v", live, err)
	}
}

// A live store that has opened no git remote has no base, and its clone
// starts with none.
func TestACloneOfAStoreWithNoMirrorsStartsWithNone(t *testing.T) {
	t.Parallel()
	mirrorRoot := t.TempDir()
	cloneRoot := filepath.Join(t.TempDir(), "clone", "dolt")
	if err := SeedCloneMirrors(context.Background(), mirrorRoot, filepath.Join(t.TempDir(), "dolt"), cloneRoot); err != nil {
		t.Fatalf("SeedCloneMirrors() error = %v", err)
	}
	if _, err := os.Stat(MirrorBase(mirrorRoot, cloneRoot)); !os.IsNotExist(err) {
		t.Fatalf("a clone of a store with no mirrors has a base (%v)", err)
	}
}

// An older lit's in-repository cache is removed by the prune.
func TestThePruneRemovesAnOlderLitsInRepositoryCache(t *testing.T) {
	t.Parallel()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	legacy := filepath.Join(legacyRemoteCacheBase(doltRoot), strings.Repeat("b", 64), "repo.git")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "HEAD"), []byte("ref: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outcome := collectLegacyRemoteCache(doltRoot)
	if outcome.Problem != "" || outcome.Removed != 1 || outcome.Reclaimed == 0 {
		t.Fatalf("collect = %+v, want the cache removed and its bytes reclaimed", outcome)
	}
	if _, err := os.Stat(legacyRemoteCacheBase(doltRoot)); !os.IsNotExist(err) {
		t.Fatalf("the in-repository cache still exists (%v)", err)
	}
}
