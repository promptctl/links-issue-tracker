// Package fsperm holds what a test needs to know about filesystem permissions
// on the platform it runs on: faults that take a permission away, and the
// mode a requested permission reads back as.
//
// A directory's mode bits are the fault on POSIX, and they do not hold
// everywhere: root bypasses them, and Windows does not enforce a directory's
// mode at all. So each fault is measured once it is set, and a test runs where
// it holds and is skipped, saying why, where it does not — never run against a
// fault that silently did nothing. Windows keeps a single read-only bit where
// POSIX keeps nine, so a mode a test asserts is the one the platform stores.
// [LAW:single-enforcer] This is the one place a test learns either.
package fsperm

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// DenyWrite takes write permission off dir, so nothing can be created in it,
// renamed into or out of it, or removed from it, and restores dir's mode when
// the test ends. restore puts the mode back sooner, for a test that goes on to
// show what happens once the fault is gone; calling it twice is harmless.
func DenyWrite(t testing.TB, dir string) (restore func()) {
	t.Helper()
	restore = set(t, dir, 0o555)
	probe, err := os.CreateTemp(dir, "fsperm-probe-*")
	if err == nil {
		name := probe.Name()
		closeErr := probe.Close()
		if err := errors.Join(closeErr, os.Remove(name)); err != nil {
			t.Fatalf("remove the write probe %s: %v", name, err)
		}
		restore()
		t.Skipf("%s took a new file with its write permission off: this process bypasses directory permissions (root does, and Windows does not enforce them), so the fault cannot be injected", dir)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("probe %s for the write fault: %v; want a permission error", dir, err)
	}
	return restore
}

// DenyRead takes every permission off dir, so its entries cannot be listed,
// and restores dir's mode when the test ends, as DenyWrite does.
func DenyRead(t testing.TB, dir string) (restore func()) {
	t.Helper()
	restore = set(t, dir, 0o000)
	_, err := os.ReadDir(dir)
	if err == nil {
		restore()
		t.Skipf("%s listed with every permission off: this process bypasses directory permissions (root does, and Windows does not enforce them), so the fault cannot be injected", dir)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("probe %s for the read fault: %v; want a permission error", dir, err)
	}
	return restore
}

// set changes dir's mode to perm and returns what puts the old mode back,
// registered to run when the test ends too, so the test's temporary directory
// can still be removed. A directory no longer at dir was moved by the code
// under test, and the test that knows where it went restores it; one left
// unwritable makes removing the temporary directory fail, so that is not
// silent either.
func set(t testing.TB, dir string, perm fs.FileMode) (restore func()) {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s before injecting a permission fault: %v", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory; a permission fault here is a directory's", dir)
	}
	if err := os.Chmod(dir, perm); err != nil {
		t.Fatalf("chmod %s %v: %v", dir, perm, err)
	}
	restore = func() {
		if err := os.Chmod(dir, info.Mode().Perm()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("restore %s to %v: %v", dir, info.Mode().Perm(), err)
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
