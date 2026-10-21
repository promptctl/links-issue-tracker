package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// PrimaryClone tells the repository's primary clone from a linked worktree,
// from anywhere inside either. Reached through a symlink, the primary clone is
// recognised only when the two git directories are compared as physical paths;
// the test makes its own symlink, since only macOS puts one in t.TempDir's path
// (/var -> /private/var).
func TestResolveTellsThePrimaryCloneFromALinkedWorktree(t *testing.T) {
	primary := litRepoWithCommit(t)
	subdir := filepath.Join(primary, "nested", "deeper")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	run(t, primary, "git", "worktree", "add", "-b", "linked", linked)
	viaSymlink := filepath.Join(t.TempDir(), "via-symlink")
	if err := os.Symlink(primary, viaSymlink); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		cwd  string
		want bool
	}{
		{"the primary clone's root", primary, true},
		{"a directory inside the primary clone", subdir, true},
		{"the primary clone reached through a symlink", viaSymlink, true},
		{"a linked worktree", linked, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := Resolve(tc.cwd)
			if err != nil {
				t.Fatalf("Resolve(%q) error = %v", tc.cwd, err)
			}
			if info.PrimaryClone() != tc.want {
				t.Errorf("Resolve(%q).PrimaryClone() = %v, want %v (git-dir %q, common dir %q)", tc.cwd, info.PrimaryClone(), tc.want, info.PrivateGitDir, info.GitCommonDir)
			}
		})
	}
}
