package fsperm

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestDenyWriteRefusesANewEntryUntilRestored(t *testing.T) {
	dir := t.TempDir()
	restore := DenyWrite(t, dir)
	name := filepath.Join(dir, "entry")
	if err := os.WriteFile(name, nil, 0o644); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("write into a denied directory = %v; want a permission error", err)
	}
	restore()
	if err := os.WriteFile(name, nil, 0o644); err != nil {
		t.Fatalf("write once restored = %v; want it to land", err)
	}
	assertOnlyEntry(t, dir, "entry")
}

func TestDenyReadRefusesAListingUntilRestored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "entry"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	restore := DenyRead(t, dir)
	if _, err := os.ReadDir(dir); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("list a denied directory = %v; want a permission error", err)
	}
	restore()
	assertOnlyEntry(t, dir, "entry")
}

// assertOnlyEntry also shows the write probe left nothing behind.
func assertOnlyEntry(t *testing.T, dir, want string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != want {
		t.Fatalf("%s holds %v, want only %s", dir, entries, want)
	}
}
