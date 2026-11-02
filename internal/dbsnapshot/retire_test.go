package dbsnapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/fsperm"
)

// newTakenSource makes a database dir with one file, ready to Take from.
func newTakenSource(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

// takeUndeletable takes a snapshot and plants a subtree in it that RemoveAll
// cannot clear, so retiring it fails part-way. The cleanup restores the
// subtree wherever the snapshot has been renamed to.
func takeUndeletable(t *testing.T, src string) Snapshot {
	t.Helper()
	taken, err := Take(context.Background(), os.Stderr, src, "")
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(taken.Path, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "partial"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsperm.Deny(t, nested, fsperm.RemoveAll)
	t.Cleanup(func() { restoreCondemnedPerms(t, DirFor(src)) })
	return taken.Snapshot
}

func listNames(t *testing.T, src string) []string {
	t.Helper()
	list, err := List(src)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list))
	for _, s := range list {
		names = append(names, s.Name)
	}
	return names
}

// TestRemove_PartialRemovalIsNeitherListedNorRestorable pins the contract: a
// snapshot whose removal fails part-way is gone as far as List and Restore can
// tell, and the error names where its remains are.
func TestRemove_PartialRemovalIsNeitherListedNorRestorable(t *testing.T) {
	t.Parallel()
	src := newTakenSource(t)
	snap := takeUndeletable(t, src)

	err := Remove(snap)
	if err == nil {
		t.Fatal("Remove must fail when the snapshot cannot be fully deleted")
	}
	if !strings.Contains(err.Error(), "condemned") {
		t.Fatalf("error %q must name the retired remains", err.Error())
	}
	if names := listNames(t, src); len(names) != 0 {
		t.Fatalf("a half-deleted snapshot must not be listed, got %v", names)
	}
	if _, err := Restore(src, snap.Name); !errors.Is(err, ErrSnapshotMissing) {
		t.Fatalf("Restore of a half-deleted snapshot: want ErrSnapshotMissing, got %v", err)
	}
}

// TestPruneMatching_GoesOnPastASnapshotThatWillNotGo pins that one stuck
// snapshot does not keep the ones behind it over budget, and that the failure
// still reaches the caller.
func TestPruneMatching_GoesOnPastASnapshotThatWillNotGo(t *testing.T) {
	t.Parallel()
	src := newTakenSource(t)
	oldest, err := Take(context.Background(), os.Stderr, src, "")
	if err != nil {
		t.Fatal(err)
	}
	stuck := takeUndeletable(t, src)
	newest, err := Take(context.Background(), os.Stderr, src, "")
	if err != nil {
		t.Fatal(err)
	}

	if err := Prune(src, 1); err == nil {
		t.Fatal("Prune must report the snapshot it could not remove")
	}
	names := listNames(t, src)
	if len(names) != 1 || names[0] != newest.Name {
		t.Fatalf("want only the newest snapshot %s listed, got %v", newest.Name, names)
	}
	if _, err := os.Stat(oldest.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the snapshot behind the stuck one must be pruned, stat err=%v", err)
	}
	if _, err := os.Stat(stuck.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stuck snapshot must be gone from its own name, stat err=%v", err)
	}
}

// TestTake_CollectsAHalfDeletedSnapshotOnceItCanBeDeleted pins the retry: the
// next Take's collector reaps the retired remains, with no one remembering to.
func TestTake_CollectsAHalfDeletedSnapshotOnceItCanBeDeleted(t *testing.T) {
	t.Parallel()
	src := newTakenSource(t)
	snap := takeUndeletable(t, src)
	if err := Remove(snap); err == nil {
		t.Fatal("Remove must fail while the subtree is undeletable")
	}
	restoreCondemnedPerms(t, DirFor(src))

	if _, err := Take(context.Background(), os.Stderr, src, ""); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(DirFor(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if IsProducerArtifactName(e.Name()) {
			t.Fatalf("the next Take must collect the retired remains, found %s", e.Name())
		}
	}
}

// TestRemove_LegacyLongLabelStillRemovable pins that retirement does not depend
// on the label's length: parseName accepts labels longer than any lit mints
// today, and such a snapshot must stay removable.
func TestRemove_LegacyLongLabelStillRemovable(t *testing.T) {
	t.Parallel()
	src := newTakenSource(t)
	name := "1700000000000000001-" + strings.Repeat("a", 230)
	path := filepath.Join(DirFor(src), name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if names := listNames(t, src); len(names) != 1 || names[0] != name {
		t.Fatalf("setup: legacy snapshot must be listed, got %v", names)
	}

	list, err := List(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(list[0]); err != nil {
		t.Fatalf("Remove of a long-label snapshot: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot must be gone, stat err=%v", err)
	}
}
