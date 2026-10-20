// Package fsperm holds what a test needs to know about filesystem permissions
// on the platform it runs on: faults that take a permission away, and the
// mode a requested permission reads back as.
//
// Mode bits are the fault on POSIX, and they do not hold everywhere: root
// bypasses them, and Windows enforces only part of them (a file's read-only
// bit, and a directory's against os.Remove of the directory). So each fault
// is measured once it is set, and a test runs where it holds and is skipped,
// saying why, where it does not — never run against a fault that silently did
// nothing.
// Windows keeps a single read-only bit where POSIX keeps nine, so a mode a
// test asserts is the one the platform stores.
// [LAW:single-enforcer] This is the one place a test learns either.
package fsperm

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// An Op is an operation on a directory that Deny makes it refuse.
type Op int

const (
	// Write changes the directory's entries: creating one, renaming one into
	// or out of it, or removing one from it.
	Write Op = iota
	// RemoveAll removes the directory with what it holds by os.RemoveAll,
	// which on Windows deletes a read-only directory.
	RemoveAll
	// Remove removes the directory's entries and then the directory itself by
	// os.Remove, which Windows refuses for a read-only directory.
	Remove
	// Read lists the directory's entries.
	Read
)

func (op Op) String() string {
	return [...]string{Write: "a new entry", RemoveAll: "its os.RemoveAll", Remove: "its os.Remove", Read: "a listing"}[op]
}

// Deny takes permission off dir so it refuses op, and restores dir's mode when
// the test ends. restore puts the mode back sooner, for a test that goes on to
// show what happens once the fault is gone; calling it twice is harmless.
// [LAW:one-type-per-behavior] Every fault is a mode change on dir; the caller
// names the call its code under test makes on dir, because the platforms
// enforce the mode apart by call, and the measurement makes that same call.
func Deny(t testing.TB, dir string, op Op) (restore func()) {
	t.Helper()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("%s is not a directory to deny %s on: %v", dir, op, err)
	}
	restore, err := op.inject(t, dir)
	if err == nil {
		restore()
		t.Skipf("%s did not refuse %s with its permission off: this process bypasses directory permissions (root does, and Windows enforces a directory's read-only mode only against os.Remove of the directory), so the fault cannot be injected", dir, op)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("probe %s for refusing %s: %v; want a permission error", dir, op, err)
	}
	return restore
}

// inject sets op's mode on dir and attempts op where the attempt cannot harm
// dir, returning what it met.
func (op Op) inject(t testing.TB, dir string) (restore func(), attempt error) {
	t.Helper()
	switch op {
	case Write:
		restore = set(t, dir, 0o555)
		probe, err := os.CreateTemp(dir, "fsperm-probe-*")
		if err == nil {
			name := probe.Name()
			if err := errors.Join(probe.Close(), os.Remove(name)); err != nil {
				t.Fatalf("remove the write probe %s: %v", name, err)
			}
		}
		return restore, err
	case RemoveAll, Remove:
		// On POSIX a directory's own mode guards only its entries; an empty
		// one is removed on its parent's permission.
		if entries, err := os.ReadDir(dir); err != nil || len(entries) == 0 {
			t.Fatalf("%s must hold an entry to refuse its removal on every platform: entries %v, %v", dir, entries, err)
		}
		// Removing dir itself would take its contents, so the attempt is on
		// a twin holding one file.
		twin := filepath.Join(t.TempDir(), "fsperm-removal-probe")
		if err := os.Mkdir(twin, 0o755); err != nil {
			t.Fatal(err)
		}
		entry := filepath.Join(twin, "entry")
		if err := os.WriteFile(entry, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		set(t, twin, 0o555)
		restore = set(t, dir, 0o555)
		if op == RemoveAll {
			return restore, os.RemoveAll(twin)
		}
		if err := os.Remove(entry); err != nil {
			return restore, err
		}
		return restore, os.Remove(twin)
	case Read:
		restore = set(t, dir, 0o000)
		_, err := os.ReadDir(dir)
		return restore, err
	}
	t.Fatalf("fsperm has no fault for %d", int(op))
	return nil, nil
}

// RequireReadOnly skips the test unless file, whose write permission the
// caller has taken off, refuses to be opened for writing: root opens it
// anyway. Windows enforces a file's read-only bit, so the test runs there.
func RequireReadOnly(t testing.TB, file string) {
	t.Helper()
	f, err := os.OpenFile(file, os.O_WRONLY, 0)
	if err == nil {
		if err := f.Close(); err != nil {
			t.Fatalf("close the write probe of %s: %v", file, err)
		}
		t.Skipf("%s opened for writing with its write permission off: this process bypasses file permissions (root does)", file)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("probe %s for the read-only fault: %v; want a permission error", file, err)
	}
}

// FreezeTree makes the tree at root read-only until the test ends: every
// directory 0o555 and every file 0o444. It is measured on a file, whose
// read-only bit every platform enforces and root bypasses; the directories
// refuse new entries too on POSIX, and do not on Windows, so a test of a
// frozen tree asserts only that a write into it fails.
func FreezeTree(t testing.TB, root string) {
	t.Helper()
	var file string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			set(t, path, 0o555)
			return nil
		}
		set(t, path, 0o444)
		file = path
		return nil
	}); err != nil {
		t.Fatalf("freeze %s: %v", root, err)
	}
	if file == "" {
		t.Fatalf("%s holds no file to measure its freeze on", root)
	}
	RequireReadOnly(t, file)
}

// set changes path's mode to perm and returns what puts the old mode back,
// registered to run when the test ends too, so the test's temporary directory
// can still be removed. A path no longer there was moved by the code under
// test, and the test that knows where it went restores it; a directory left
// unwritable makes removing the temporary directory fail, so that is not
// silent either.
func set(t testing.TB, path string, perm fs.FileMode) (restore func()) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s before injecting a permission fault: %v", path, err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod %s %v: %v", path, perm, err)
	}
	restore = func() {
		if err := os.Chmod(path, info.Mode().Perm()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("restore %s to %v: %v", path, info.Mode().Perm(), err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// FileModeAsStored is the mode a file chmodded to perm reads back with: perm
// on POSIX, and 0o666 or 0o444 on Windows. It chmods a file of its own, by
// name, so it measures the platform and not the code a test is checking.
func FileModeAsStored(t testing.TB, perm fs.FileMode) fs.FileMode {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "mode-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return chmodded(t, probe, perm)
}

// DirModeAsStored is FileModeAsStored for a directory.
func DirModeAsStored(t testing.TB, perm fs.FileMode) fs.FileMode {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "mode-probe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	mode := chmodded(t, probe, perm)
	// Back to writable, so the test's temporary directory can be removed.
	if err := os.Chmod(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	return mode
}

func chmodded(t testing.TB, probe string, perm fs.FileMode) fs.FileMode {
	t.Helper()
	if err := os.Chmod(probe, perm); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
