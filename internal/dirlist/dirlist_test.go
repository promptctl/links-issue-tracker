package dirlist

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestRead_RefusesFileWithENOTDIR(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "owned")
	if err := os.WriteFile(path, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := Read(path)
	var pathErr *fs.PathError
	if !errors.Is(err, syscall.ENOTDIR) || !errors.As(err, &pathErr) || pathErr.Path != path {
		t.Fatalf("Read(file) = %d entries, %v; want *fs.PathError for %s wrapping ENOTDIR", len(entries), err, path)
	}
}

func TestRead_ListsSortedEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"b", "c", "a"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Fatalf("Read names = %v, want %v", got, want)
	}
}

func TestRead_MissingIsNotExist(t *testing.T) {
	t.Parallel()
	if _, err := Read(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read(missing) err = %v, want fs.ErrNotExist", err)
	}
}
