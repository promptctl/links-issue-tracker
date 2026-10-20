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

// Each removal fault is measured by the call it names, and the code under test
// making that call meets it.
func TestDenyRemovalKeepsTheDirectoryUntilRestored(t *testing.T) {
	removals := map[Op]func(dir string) error{
		RemoveAll: os.RemoveAll,
		Remove: func(dir string) error {
			if err := os.Remove(filepath.Join(dir, "entry")); err != nil {
				return err
			}
			return os.Remove(dir)
		},
	}
	for op, remove := range removals {
		t.Run(op.String(), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "held")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "entry"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			restore := Deny(t, dir, op)
			if err := remove(dir); !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("remove a denied directory = %v; want a permission error", err)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("the denied directory is gone: %v", err)
			}
			restore()
			if err := os.RemoveAll(dir); err != nil {
				t.Fatalf("remove once restored = %v; want it gone", err)
			}
		})
	}
}

// On POSIX an empty directory is removed on its parent's permission, whatever
// its own mode, so its removal cannot be refused on every platform.
func TestDenyRemovalFailsOnAnEmptyDirectory(t *testing.T) {
	for _, op := range []Op{RemoveAll, Remove} {
		dir := t.TempDir()
		rec := &fatalRecorder{TB: t}
		done := make(chan struct{})
		go func() {
			defer close(done)
			Deny(rec, dir, op)
		}()
		<-done
		if !strings.Contains(rec.fatal, "must hold an entry") {
			t.Fatalf("Deny(empty dir, %v) fatal = %q; want it refused for holding nothing", op, rec.fatal)
		}
	}
}

func TestFreezeTreeRefusesAFileWriteUntilTheTestEnds(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "sub", "file")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Run("frozen", func(t *testing.T) {
		FreezeTree(t, root)
		if err := os.WriteFile(file, []byte("lost"), 0o644); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("write a frozen file = %v; want a permission error", err)
		}
	})
	if err := os.WriteFile(file, []byte("thawed"), 0o644); err != nil {
		t.Fatalf("write once the freezing test ended = %v; want it to land", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "new"), nil, 0o644); err != nil {
		t.Fatalf("create once the freezing test ended = %v; want it to land", err)
	}
}

// A tree with no file has nothing whose freeze every platform enforces.
func TestFreezeTreeFailsOnATreeWithNoFile(t *testing.T) {
	root := t.TempDir()
	rec := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		FreezeTree(rec, root)
	}()
	<-done
	if !strings.Contains(rec.fatal, "holds no file") {
		t.Fatalf("FreezeTree(no file) fatal = %q; want it refused for holding no file", rec.fatal)
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
