package fsperm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDenyWriteRefusesANewEntryUntilRestored(t *testing.T) {
	dir := t.TempDir()
	restore := Deny(t, dir, Write)
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
	restore := Deny(t, dir, Read)
	if _, err := os.ReadDir(dir); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("list a denied directory = %v; want a permission error", err)
	}
	restore()
	assertOnlyEntry(t, dir, "entry")
}

func TestDenyRemovalKeepsTheDirectoryUntilRestored(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "held")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	restore := Deny(t, dir, Removal)
	if err := os.RemoveAll(dir); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("remove a denied directory = %v; want a permission error", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the denied directory is gone: %v", err)
	}
	restore()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove once restored = %v; want it gone", err)
	}
}

// On POSIX an empty directory is removed on its parent's permission, whatever
// its own mode, so its removal cannot be refused on every platform.
func TestDenyRemovalFailsOnAnEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	rec := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Deny(rec, dir, Removal)
	}()
	<-done
	if !strings.Contains(rec.fatal, "must hold an entry") {
		t.Fatalf("Deny(empty dir, Removal) fatal = %q; want it refused for holding nothing", rec.fatal)
	}
}

// fatalRecorder records a Fatalf and ends the goroutine that called it, as
// testing.T does, without failing the test it wraps.
type fatalRecorder struct {
	testing.TB
	fatal string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func TestRequireReadOnlyRunsWhereAReadOnlyFileRefusesAWrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "frozen")
	if err := os.WriteFile(file, []byte("kept"), 0o444); err != nil {
		t.Fatal(err)
	}
	RequireReadOnly(t, file)
	if err := os.WriteFile(file, []byte("lost"), 0o444); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("write a read-only file = %v; want a permission error", err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
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
