//go:build unix

package dbsnapshot

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestCloneTree_PreservesPermsUnderRestrictiveUmask lives in a unix-only test
// file because syscall.Umask is not defined on Windows. The Windows build of
// the package has its own cloneTree (plain copy + Chmod) but no umask concept
// to defend against.
func TestCloneTree_PreservesPermsUnderRestrictiveUmask(t *testing.T) {
	// Not t.Parallel — syscall.Umask is process-wide and racy under parallel tests.
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })

	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "nested", "f"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := cloneTree(src, dst); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Join(dst, "nested"))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o755 {
		t.Fatalf("nested dir perm = %v, want 0755 (umask 077 must not affect snapshot)", dirInfo.Mode().Perm())
	}
	fileInfo, err := os.Stat(filepath.Join(dst, "nested", "f"))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o644 {
		t.Fatalf("file perm = %v, want 0644 (umask 077 must not affect snapshot)", fileInfo.Mode().Perm())
	}
}
