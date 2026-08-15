//go:build unix

package dbsnapshot

import (
	"context"
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
	if err := cloneTree(context.Background(), src, dst); err != nil {
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

// TestTake_CloneFailureLeavesNoResidue drives the same cleanup path a
// ctx-canceled copy takes — cloneTree fails partway, the .tmp is removed, the
// .reserve released — using a FIFO (an unsupported entry type) as the
// deterministic mid-tree failure. On Darwin the FIFO also forces the
// Clonefile fast path to be irrelevant: even if the syscall clones it, the
// walk fallback on other filesystems must behave identically, and the
// contract asserted here is only "a failed Take strands nothing".
func TestTake_CloneFailureLeavesNoResidue(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(src, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotsDir := filepath.Join(root, "snapshots")
	_, err := Take(context.Background(), src, snapshotsDir, "")
	if err == nil {
		// Darwin's Clonefile clones FIFOs wholesale; only the walk-based
		// paths refuse them. Either outcome is legal — the residue contract
		// below is what this test pins.
		t.Log("Take succeeded (CoW fast path clones special files)")
	}
	entries, readErr := os.ReadDir(snapshotsDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		if isProducerArtifactName(e.Name()) {
			t.Fatalf("failed Take stranded producer artifact: %s", e.Name())
		}
	}
}
