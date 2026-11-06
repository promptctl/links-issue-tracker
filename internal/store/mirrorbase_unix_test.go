//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// A base whose mirror will not go keeps its root file, so the next collection
// still finds it and retries instead of leaving its bytes on disk for good.
func TestARemovalThatFailsPartwayLeavesABaseTheNextCollectionFinds(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root removes what a read-only directory holds")
	}
	mirrorRoot := t.TempDir()
	repo := filepath.Join(t.TempDir(), "gone")
	base := writeBase(t, mirrorRoot, filepath.Join(repo, "dolt"))
	stuck := filepath.Join(base, "stuck")
	if err := os.MkdirAll(stuck, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "pack"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o755) })

	if outcome := sweepOrphanedMirrorBases(mirrorRoot); outcome.Problem == "" {
		t.Fatalf("sweep = %+v, want the stuck mirror reported", outcome)
	}
	bases, err := readMirrorBases(mirrorRoot)
	if err != nil || len(bases) != 1 || bases[0].dir != base {
		t.Fatalf("bases after a failed removal = %+v (%v), want %s still listed", bases, err, base)
	}

	if err := os.Chmod(stuck, 0o755); err != nil {
		t.Fatal(err)
	}
	if outcome := sweepOrphanedMirrorBases(mirrorRoot); outcome.Problem != "" || outcome.Removed != 1 {
		t.Fatalf("retried sweep = %+v, want the base removed", outcome)
	}
}
