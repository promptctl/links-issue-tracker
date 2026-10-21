package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// MainCheckout tells the repository's main working tree from a linked worktree,
// from anywhere inside either. t.TempDir sits under a symlinked directory on
// macOS (/var -> /private/var), so the main clone is only recognised when the
// two git directories are compared as physical paths.
func TestResolveTellsTheMainCheckoutFromALinkedWorktree(t *testing.T) {
	primary := litRepoWithCommit(t)
	subdir := filepath.Join(primary, "nested", "deeper")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	run(t, primary, "git", "worktree", "add", "-b", "linked", linked)

	for _, tc := range []struct {
		name string
		cwd  string
		want bool
	}{
		{"the main clone's root", primary, true},
		{"a directory inside the main clone", subdir, true},
		{"a linked worktree", linked, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := Resolve(tc.cwd)
			if err != nil {
				t.Fatalf("Resolve(%q) error = %v", tc.cwd, err)
			}
			if info.MainCheckout != tc.want {
				t.Errorf("Resolve(%q).MainCheckout = %v, want %v (git-dir %q, common dir %q)", tc.cwd, info.MainCheckout, tc.want, info.PrivateGitDir, info.GitCommonDir)
			}
		})
	}
}
