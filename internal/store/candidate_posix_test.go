//go:build !windows

package store

import (
	"context"
	"os"
	"testing"
)

// TestDiscardRetriesDirectoryRemoval proves the zero-residue guarantee survives
// a transient filesystem failure. With the parent dir made unwritable, the first
// Discard closes the store but cannot unlink the candidate root, so it errors and
// keeps root tracked; once the parent is writable again a second Discard
// re-attempts removal and clears it. A single shared release flag would have
// nulled everything on the first attempt and the retry would no-op, stranding the
// directory.
//
// The failure is injected via POSIX directory-write-bit semantics (removing an
// entry needs write permission on its parent), so the test is !windows by
// construction. The root-skip is an orthogonal axis: root bypasses the permission
// check, so the injection cannot fail there.
func TestDiscardRetriesDirectoryRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("removal-permission injection has no effect as root")
	}
	ctx := context.Background()
	parent := t.TempDir()
	dump := preGooseDump()

	cand, err := RebuildCandidate(ctx, parent, dump, mustMap(t, dump))
	if err != nil {
		t.Fatalf("RebuildCandidate: %v", err)
	}
	t.Cleanup(func() { _ = cand.Discard() })

	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	// Always restore write permission, even on an early Fatalf, so a stranded
	// unwritable parent cannot break t.TempDir teardown. Idempotent with the
	// explicit restore below.
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	if err := cand.Discard(); err == nil {
		t.Fatal("Discard succeeded despite an unwritable parent; expected a removal error")
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatalf("restore parent perms: %v", err)
	}

	if err := cand.Discard(); err != nil {
		t.Fatalf("retry Discard did not clear residue: %v", err)
	}
	if n := dirEntryCount(t, parent); n != 0 {
		t.Fatalf("directory residue survived retry: %d entries remain", n)
	}
}
