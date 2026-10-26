package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// git prints rev-parse's answers one per line, unquoted, so a path holding a
// newline arrives as more lines than answers. The cut that names three
// existing directories is the answer; no such cut, or more than one, is
// refused.
func TestParseGeometryTakesTheOneCutThatNamesExistingDirectories(t *testing.T) {
	base := t.TempDir()
	mk := func(path string) string {
		t.Helper()
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", path, err)
		}
		return path
	}
	plainRoot, plainGit := mk(filepath.Join(base, "plain")), mk(filepath.Join(base, "plain", ".git"))
	newlineRoot := mk(filepath.Join(base, "wt\nprunable"))
	newlineGit := mk(filepath.Join(base, "repo\nb", ".git"))
	// Two cuts of one output that both name existing directories.
	a, b, c, d := filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "c"), filepath.Join(base, "d")
	for _, dir := range []string{a, b, c + "\n" + d, a + "\n" + b, c, d} {
		mk(dir)
	}

	for _, tc := range []struct {
		name   string
		cwd    string
		out    string
		want   geometry
		refuse string
	}{
		{"three lines", plainRoot, plainRoot + "\n.git\n.git\n", geometry{root: plainRoot, commonDir: plainGit, gitDir: plainGit}, ""},
		{"a newline in the toplevel", newlineRoot, newlineRoot + "\n" + plainGit + "\n" + plainGit + "\n", geometry{root: newlineRoot, commonDir: plainGit, gitDir: plainGit}, ""},
		{"a newline in the git dirs", plainRoot, plainRoot + "\n" + newlineGit + "\n" + newlineGit + "\n", geometry{root: plainRoot, commonDir: newlineGit, gitDir: newlineGit}, ""},
		{"no cut names directories", plainRoot, plainRoot + "\n.git\n" + filepath.Join(base, "gone") + "\n", geometry{}, "names no working-tree root"},
		{"two cuts name directories", base, a + "\n" + b + "\n" + c + "\n" + d + "\n", geometry{}, "2 different sets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGeometry(tc.cwd, tc.out)
			if tc.refuse != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refuse) {
					t.Fatalf("parseGeometry() = %+v, %v; want a refusal containing %q", got, err, tc.refuse)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseGeometry() = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

// Resolve takes the checkout it runs in from git's own answer, so a GIT_DIR
// git honours is honoured: from a subdirectory under a hook's GIT_DIR, and for
// a repository whose git dir lives apart from its working tree.
func TestResolveFollowsTheGitDirGitIsGiven(t *testing.T) {
	repo := litRepoWithCommit(t)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	apart := filepath.Join(t.TempDir(), "dotfiles.git")
	run(t, home, "git", "init", "--bare", apart)
	for _, tc := range []struct {
		name, cwd, gitDir, workTree string
	}{
		{"a subdirectory under the hook's GIT_DIR", sub, filepath.Join(repo, ".git"), ""},
		{"a git dir apart from its working tree", home, apart, home},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GIT_DIR", tc.gitDir)
			if tc.workTree != "" {
				t.Setenv("GIT_WORK_TREE", tc.workTree)
			}
			info, err := Resolve(testEnv(), tc.cwd)
			if err != nil {
				t.Fatalf("Resolve(%q) error = %v", tc.cwd, err)
			}
			if want := physicalPath(t, tc.gitDir); info.PrivateGitDir != want {
				t.Fatalf("Resolve(%q).PrivateGitDir = %q, want git's %q", tc.cwd, info.PrivateGitDir, want)
			}
		})
	}
}

// A `.git` entry is read in the two forms git writes it: the git dir itself,
// or a gitfile read whole, as git reads one.
func TestCheckoutGitDirReadsTheDotGitEntry(t *testing.T) {
	base := t.TempDir()
	physical := func(path string) string {
		t.Helper()
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", path, err)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q) error = %v", path, err)
		}
		return resolved
	}
	target := physical(filepath.Join(base, "admin", "worktrees", "lw"))
	newlineTarget := physical(filepath.Join(base, "repo\nb", ".git", "worktrees", "lw"))
	withGitfile := func(content string) string {
		t.Helper()
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".git"), []byte(content), 0o644); err != nil {
			t.Fatalf("write gitfile: %v", err)
		}
		return root
	}
	primary := t.TempDir()
	primaryGitDir := physical(filepath.Join(primary, ".git"))
	relativeRoot := filepath.Join(base, "relative-root")
	if err := os.Mkdir(relativeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relativeRoot, ".git"), []byte("gitdir: ../admin/worktrees/lw\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		root   string
		want   string
		refuse string
	}{
		{"a .git directory", primary, primaryGitDir, ""},
		{"an absolute gitfile", withGitfile("gitdir: " + target + "\n"), target, ""},
		{"a relative gitfile", relativeRoot, target, ""},
		{"a CRLF gitfile", withGitfile("gitdir: " + target + "\r\n"), target, ""},
		{"a newline inside the named path", withGitfile("gitdir: " + newlineTarget + "\n"), newlineTarget, ""},
		{"no gitdir prefix", withGitfile(target + "\n"), "", "does not name a git dir"},
		{"an empty path", withGitfile("gitdir: \n"), "", "does not name a git dir"},
		{"no .git at all", t.TempDir(), "", "read the .git entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := checkoutGitDir(tc.root)
			if tc.refuse != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refuse) {
					t.Fatalf("checkoutGitDir() = %q, %v; want a refusal containing %q", got, err, tc.refuse)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("checkoutGitDir() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// The identity Resolve gives a checkout is the identity the enumeration finds
// at that checkout's path, for every shape of checkout git makes — so a
// checkout's own claims are never voided as if it had been deleted.
func TestResolveAndTheEnumerationAgreeOnEveryCheckoutsIdentity(t *testing.T) {
	primary := litRepoWithCommit(t)
	linked := filepath.Join(t.TempDir(), "linked")
	run(t, primary, "git", "worktree", "add", "-b", "linked", linked)
	// git older than 2.48 ignores the setting and writes an absolute gitfile,
	// which the case above already covers.
	relative := filepath.Join(primary, "..", filepath.Base(primary)+"-relative")
	run(t, primary, "git", "-c", "worktree.useRelativePaths=true", "worktree", "add", "-b", "relative", relative)
	newlineLinked := dirNamedExactly(t, t.TempDir(), "wt\nlinked")
	if err := os.Remove(newlineLinked); err != nil {
		t.Fatal(err)
	}
	run(t, primary, "git", "worktree", "add", "-b", "newline", newlineLinked)

	newlinePrimary := dirNamedExactly(t, t.TempDir(), "repo\nprimary")
	run(t, newlinePrimary, "git", "init")
	run(t, newlinePrimary, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	linkedOfNewline := filepath.Join(t.TempDir(), "of-newline")
	run(t, newlinePrimary, "git", "worktree", "add", "-b", "of-newline", linkedOfNewline)

	// In both layouts below git lists the main worktree at its git dir, which
	// holds no `.git` entry: the enumeration finds the identity there anyway.
	separate := filepath.Join(t.TempDir(), "separate")
	separateGitDir := filepath.Join(t.TempDir(), "separate.git")
	run(t, filepath.Dir(separate), "git", "init", "--separate-git-dir="+separateGitDir, separate)
	run(t, separate, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	linkedOfSeparate := filepath.Join(t.TempDir(), "of-separate")
	run(t, separate, "git", "worktree", "add", "-b", "of-separate", linkedOfSeparate)

	super := litRepoWithCommit(t)
	run(t, super, "git", "-c", "protocol.file.allow=always", "submodule", "add", primary, "sub")
	submodule := filepath.Join(super, "sub")
	submoduleGitDir := filepath.Join(super, ".git", "modules", "sub")

	agree := func(t *testing.T, enumerateFrom string, checkout string, listedAt string) {
		t.Helper()
		token := tokenOf(t, checkout)
		checkouts, err := LiveCheckouts(testEnv(), resolved(t, enumerateFrom))
		if err != nil {
			t.Fatalf("LiveCheckouts(%q) error = %v", enumerateFrom, err)
		}
		at := slices.IndexFunc(checkouts, func(c Checkout) bool { return c.Stream.Present() && c.Stream.Value() == token })
		if at < 0 {
			t.Fatalf("token %q that Resolve gave %q is not in the enumeration %+v", token, checkout, checkouts)
		}
		if got, want := physicalPath(t, checkouts[at].Path), physicalPath(t, listedAt); got != want {
			t.Fatalf("token %q is enumerated at %q, want where git lists the checkout that minted it, %q", token, got, want)
		}
	}
	for _, tc := range []struct{ name, from, checkout, listedAt string }{
		{"the primary clone", linked, primary, primary},
		{"a linked worktree", primary, linked, linked},
		{"a relative-path worktree", primary, relative, relative},
		{"a linked worktree at a path with a newline", primary, newlineLinked, newlineLinked},
		{"a primary clone at a path with a newline", linkedOfNewline, newlinePrimary, newlinePrimary},
		{"a linked worktree of a primary clone at a path with a newline", newlinePrimary, linkedOfNewline, linkedOfNewline},
		{"a --separate-git-dir clone", separate, separate, separateGitDir},
		{"a --separate-git-dir clone, from its linked worktree", linkedOfSeparate, separate, separateGitDir},
		{"a linked worktree of a --separate-git-dir clone", separate, linkedOfSeparate, linkedOfSeparate},
		{"a submodule", submodule, submodule, submoduleGitDir},
	} {
		t.Run(tc.name, func(t *testing.T) { agree(t, tc.from, tc.checkout, tc.listedAt) })
	}
	// A git hook runs with GIT_DIR set to its own checkout's git dir; lit run
	// from the hook is that checkout, with the identity it has anywhere else.
	t.Run("under the GIT_DIR a hook exports", func(t *testing.T) {
		info, err := Resolve(testEnv(), linked)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_DIR", info.PrivateGitDir)
		agree(t, linked, linked, linked)
		underHook, err := Resolve(testEnv(), linked)
		if err != nil || underHook.PrivateGitDir != info.PrivateGitDir {
			t.Fatalf("Resolve under GIT_DIR = %q, %v; want %q", underHook.PrivateGitDir, err, info.PrivateGitDir)
		}
	})
}

func physicalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) error = %v", path, err)
	}
	return resolved
}

// Resolve asks git where the checkout is once, and the enumeration asks for
// the worktree list once, however many worktrees there are.
func TestResolveAndTheEnumerationEachSpawnOneGit(t *testing.T) {
	primary := litRepoWithCommit(t)
	for _, name := range []string{"one", "two", "three"} {
		run(t, primary, "git", "worktree", "add", "-b", name, filepath.Join(t.TempDir(), name))
	}
	env := testEnv()
	info, err := Resolve(env, primary)
	if err != nil {
		t.Fatal(err)
	}
	if got := env.GitSpawns(); got != 1 {
		t.Fatalf("Resolve spawned %d git, want 1", got)
	}
	if _, err := LiveCheckouts(env, info); err != nil {
		t.Fatal(err)
	}
	if got := env.GitSpawns(); got != 2 {
		t.Fatalf("Resolve then LiveCheckouts over four worktrees spawned %d git, want 2", got)
	}
}

// Each remote question is asked of git once however often it is read, and a
// git that cannot answer is an error, never "no upstream".
func TestRemotesAskEachQuestionOnce(t *testing.T) {
	repo := litRepoWithCommit(t)
	run(t, repo, "git", "remote", "add", "origin", "https://example.com/origin.git")
	env := testEnv()
	remotes := NewRemotes(env, testRoot(t, repo))
	for range 3 {
		if list, err := remotes.List(eventContext()); err != nil || len(list) != 1 {
			t.Fatalf("List() = %v, %v; want origin", list, err)
		}
		if upstream, err := remotes.Upstream(); err != nil || upstream != "" {
			t.Fatalf("Upstream() = %q, %v; want none", upstream, err)
		}
	}
	if got := env.GitSpawns(); got != 2 {
		t.Fatalf("three reads of each question spawned %d git, want 2", got)
	}

	notARepo := NewRemotes(testEnv(), testRoot(t, t.TempDir()))
	if upstream, err := notARepo.Upstream(); err == nil {
		t.Fatalf("Upstream() where git cannot answer = %q, nil; want git's error", upstream)
	}
}
