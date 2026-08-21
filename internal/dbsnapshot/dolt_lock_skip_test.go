package dbsnapshot

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestWalkAndCopySkipsDoltJournalLock pins the one entry the copy omits:
// Dolt's journal LOCK file. On Windows the snapshot's journal hold is a
// mandatory LockFileEx, so copying the held file through a second handle
// would fail the walk or drop the hold mid-copy; the file is contentless and
// recreated by Dolt at every open, so omitting it costs the snapshot
// nothing. Siblings must still copy — the skip is the LOCK shape, not the
// directory.
func TestWalkAndCopySkipsDoltJournalLock(t *testing.T) {
	src := t.TempDir()
	nomsDir := filepath.Join(src, "links", ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatalf("mkdir noms: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nomsDir, "LOCK"), nil, 0o600); err != nil {
		t.Fatalf("write LOCK: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nomsDir, "manifest"), []byte("m"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "copy")
	if err := walkAndCopy(context.Background(), src, dst, plainFileCopy); err != nil {
		t.Fatalf("walkAndCopy() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "links", ".dolt", "noms", "LOCK")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LOCK was copied (stat err = %v); the walk must omit it", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "links", ".dolt", "noms", "manifest")); err != nil {
		t.Fatalf("manifest sibling missing from copy: %v", err)
	}
}
