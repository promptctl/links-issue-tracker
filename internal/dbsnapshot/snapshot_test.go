package dbsnapshot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTake_RejectsMissingSource(t *testing.T) {
	t.Parallel()
	snapshotsDir := t.TempDir()
	_, err := Take(filepath.Join(snapshotsDir, "nonexistent"), snapshotsDir, "")
	if err == nil {
		t.Fatalf("Take against missing source should error")
	}
	entries, _ := os.ReadDir(snapshotsDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover .tmp directory after failed Take: %s", e.Name())
		}
	}
}

func TestTakeAndList_NewestFirst(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotsDir := filepath.Join(root, "snapshots")
	for i := 0; i < 3; i++ {
		if _, err := Take(src, snapshotsDir, ""); err != nil {
			t.Fatalf("Take %d: %v", i, err)
		}
		// Wall-clock nanosecond resolution differs by OS; insert a tiny sleep
		// so each snapshot lands at a strictly later UnixNano timestamp.
		time.Sleep(time.Millisecond)
	}
	list, err := List(snapshotsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("List len=%d, want 3", len(list))
	}
	if list[0].Created.Before(list[1].Created) || list[1].Created.Before(list[2].Created) {
		t.Fatalf("not newest-first: %v, %v, %v", list[0].Created, list[1].Created, list[2].Created)
	}
}

func TestList_IgnoresUnrecognizedNames(t *testing.T) {
	t.Parallel()
	snapshotsDir := t.TempDir()
	// Names that don't start with <digits>: leftover from prior snapshot designs.
	for _, junk := range []string{"snap-old-junk", "backup_2024", "README.txt"} {
		path := filepath.Join(snapshotsDir, junk)
		if strings.HasSuffix(junk, ".txt") {
			if err := os.WriteFile(path, []byte("note"), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	list, err := List(snapshotsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("want 0 recognized snapshots, got %d", len(list))
	}
}

func TestList_IgnoresTmpDirectories(t *testing.T) {
	t.Parallel()
	snapshotsDir := t.TempDir()
	// Simulate a crash mid-Take that left a <name>.tmp directory behind.
	if err := os.MkdirAll(filepath.Join(snapshotsDir, "1700000000000000000.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	list, err := List(snapshotsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("want 0 snapshots (only .tmp leftover), got %d", len(list))
	}
}

func TestRestore_RoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "db")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "top.txt"), []byte("top"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "deep.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotsDir := filepath.Join(root, "snapshots")
	snap, err := Take(src, snapshotsDir, "")
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the source after snapshot: delete a subtree, rewrite a file.
	if err := os.RemoveAll(filepath.Join(src, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "top.txt"), []byte("MUTATED"), 0o644); err != nil {
		t.Fatal(err)
	}
	rotated, err := Restore(src, snapshotsDir, snap.Name)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rotated == "" {
		t.Fatal("rotated path should be non-empty when src existed")
	}
	if got, err := os.ReadFile(filepath.Join(src, "top.txt")); err != nil || string(got) != "top" {
		t.Fatalf("top.txt after restore: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(src, "sub", "deep.txt")); err != nil || string(got) != "deep" {
		t.Fatalf("sub/deep.txt after restore: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(rotated, "top.txt")); err != nil || string(got) != "MUTATED" {
		t.Fatalf("rotated top.txt: %q err=%v", got, err)
	}
}

func TestRestore_SurvivesMissingDatabaseDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "db")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotsDir := filepath.Join(root, "snapshots")
	snap, err := Take(src, snapshotsDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	rotated, err := Restore(src, snapshotsDir, snap.Name)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rotated != "" {
		t.Fatalf("rotated should be empty when src absent, got %q", rotated)
	}
	if got, err := os.ReadFile(filepath.Join(src, "a.txt")); err != nil || string(got) != "a" {
		t.Fatalf("restored content: %q err=%v", got, err)
	}
}

func TestRestore_MissingSnapshotReturnsSentinel(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "db")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(src, filepath.Join(root, "snapshots"), "1700000000000000000")
	if !errors.Is(err, ErrSnapshotMissing) {
		t.Fatalf("want ErrSnapshotMissing, got %v", err)
	}
}

func TestPrune_KeepsExactlyN(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotsDir := filepath.Join(root, "snapshots")
	for i := 0; i < 7; i++ {
		if _, err := Take(src, snapshotsDir, ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := Prune(snapshotsDir, 3); err != nil {
		t.Fatal(err)
	}
	list, err := List(snapshotsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("len=%d after prune, want 3", len(list))
	}
	entries, _ := os.ReadDir(snapshotsDir)
	dirCount := 0
	for _, e := range entries {
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".tmp") {
			dirCount++
		}
	}
	if dirCount != 3 {
		t.Fatalf("on-disk dir count=%d after prune, want 3", dirCount)
	}
}

func TestPrune_RejectsNonPositive(t *testing.T) {
	t.Parallel()
	if err := Prune(t.TempDir(), 0); err == nil {
		t.Fatal("want error for keep=0")
	}
	if err := Prune(t.TempDir(), -1); err == nil {
		t.Fatal("want error for keep=-1")
	}
}

func TestPrune_EmptyDirIsNoop(t *testing.T) {
	t.Parallel()
	if err := Prune(t.TempDir(), 5); err != nil {
		t.Fatalf("Prune on empty dir: %v", err)
	}
}

func TestTake_LabelAppearsInName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	snap, err := Take(src, filepath.Join(root, "snapshots"), "pre-migration #5 / foo!")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snap.Name, "-") {
		t.Fatalf("expected sanitized label suffix in name: %s", snap.Name)
	}
	if _, err := os.Stat(snap.Path); err != nil {
		t.Fatalf("stat snapshot path: %v", err)
	}
}

func TestCloneTree_PreservesContentAndStructure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(filepath.Join(src, "nested", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "b"), []byte("beta"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "deeper", "c"), []byte("gamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cloneTree(src, dst); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		want string
	}{
		{filepath.Join(dst, "a"), "alpha"},
		{filepath.Join(dst, "nested", "b"), "beta"},
		{filepath.Join(dst, "nested", "deeper", "c"), "gamma"},
	}
	for _, tc := range cases {
		got, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if string(got) != tc.want {
			t.Fatalf("%s: %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestPlainFileCopy_PreservesPerms(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "a")
	if err := os.WriteFile(src, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "b")
	if err := plainFileCopy(src, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("perm: %v, want 0640", info.Mode().Perm())
	}
}

func TestParseName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ok   bool
	}{
		{"1700000000000000000", true},
		{"1700000000000000000-label", true},
		{"1700000000000000000-pre-migration-foo", true},
		{"snap-1700000000-abc", false},
		{"abc-def", false},
		{"", false},
		{"0", false},
	}
	for _, tc := range cases {
		_, ok := parseName(tc.name)
		if ok != tc.ok {
			t.Errorf("parseName(%q) ok=%v, want %v", tc.name, ok, tc.ok)
		}
	}
}

func TestFormatName_RoundTripsThroughParseName(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 5, 14, 12, 0, 0, 123456789, time.UTC)
	name := formatName(created, "")
	parsed, ok := parseName(name)
	if !ok {
		t.Fatalf("parseName(%q) failed", name)
	}
	if !parsed.Equal(created) {
		t.Fatalf("round trip: parsed=%v want=%v", parsed, created)
	}
}
