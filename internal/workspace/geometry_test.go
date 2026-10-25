package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// git prints rev-parse's answers one per line, unquoted, so a path holding a
// newline arrives as more lines than answers. The cut that names two existing
// directories is the answer; no such cut, or more than one, is refused.
func TestParseGeometryTakesTheOneCutThatNamesExistingDirectories(t *testing.T) {
	base := t.TempDir()
	mk := func(path string) string {
		t.Helper()
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", path, err)
		}
		return path
	}
	plainRoot, plainCommon := mk(filepath.Join(base, "plain")), mk(filepath.Join(base, "plain", ".git"))
	newlineRoot := mk(filepath.Join(base, "wt\nprunable"))
	newlineCommon := mk(filepath.Join(base, "repo\nb", ".git"))
	// Two cuts of one output that both name existing directories.
	a, b, c := filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "c")
	mk(a)
	mk(b + "\n" + c)
	mk(a + "\n" + b)
	mk(c)

	for _, tc := range []struct {
		name   string
		cwd    string
		out    string
		want   geometry
		refuse string
	}{
		{"two lines", plainRoot, plainRoot + "\n.git\n", geometry{root: plainRoot, commonDir: plainCommon}, ""},
		{"a newline in the toplevel", newlineRoot, newlineRoot + "\n" + plainCommon + "\n", geometry{root: newlineRoot, commonDir: plainCommon}, ""},
		{"a newline in the common dir", plainRoot, plainRoot + "\n" + newlineCommon + "\n", geometry{root: plainRoot, commonDir: newlineCommon}, ""},
		{"no cut names directories", plainRoot, plainRoot + "\n" + filepath.Join(base, "gone") + "\n", geometry{}, "names no working-tree root"},
		{"two cuts name directories", base, a + "\n" + b + "\n" + c + "\n", geometry{}, "2 different pairs"},
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

	agree := func(t *testing.T, enumerateFrom string, checkout string) {
		t.Helper()
		token := tokenOf(t, checkout)
		checkouts, err := LiveCheckouts(testEnv(), enumerateFrom)
		if err != nil {
			t.Fatalf("LiveCheckouts(%q) error = %v", enumerateFrom, err)
		}
		at := slices.IndexFunc(checkouts, func(c Checkout) bool { return c.Stream.Present() && c.Stream.Value() == token })
		if at < 0 {
			t.Fatalf("token %q that Resolve gave %q is not in the enumeration %+v", token, checkout, checkouts)
		}
		if got, want := physicalPath(t, checkouts[at].Path), physicalPath(t, checkout); got != want {
			t.Fatalf("token %q is enumerated at %q, want the checkout that minted it, %q", token, got, want)
		}
	}
	for _, tc := range []struct{ name, from, checkout string }{
		{"the primary clone", linked, primary},
		{"a linked worktree", primary, linked},
		{"a relative-path worktree", primary, relative},
		{"a linked worktree at a path with a newline", primary, newlineLinked},
		{"a primary clone at a path with a newline", linkedOfNewline, newlinePrimary},
		{"a linked worktree of a primary clone at a path with a newline", newlinePrimary, linkedOfNewline},
	} {
		t.Run(tc.name, func(t *testing.T) { agree(t, tc.from, tc.checkout) })
	}
	// A git hook runs with GIT_DIR set to its own checkout's git dir; lit run
	// from the hook is that checkout, with the identity it has anywhere else.
	t.Run("under the GIT_DIR a hook exports", func(t *testing.T) {
		info, err := Resolve(testEnv(), linked)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_DIR", info.PrivateGitDir)
		agree(t, linked, linked)
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
	if _, err := Resolve(env, primary); err != nil {
		t.Fatal(err)
	}
	if got := env.GitSpawns(); got != 1 {
		t.Fatalf("Resolve spawned %d git, want 1", got)
	}
	if _, err := LiveCheckouts(env, primary); err != nil {
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
	remotes := NewRemotes(env, repo)
	for range 3 {
		if list, err := remotes.List(); err != nil || len(list) != 1 {
			t.Fatalf("List() = %v, %v; want origin", list, err)
		}
		if upstream, err := remotes.Upstream(); err != nil || upstream != "" {
			t.Fatalf("Upstream() = %q, %v; want none", upstream, err)
		}
	}
	if got := env.GitSpawns(); got != 2 {
		t.Fatalf("three reads of each question spawned %d git, want 2", got)
	}

	notARepo := NewRemotes(testEnv(), t.TempDir())
	if upstream, err := notARepo.Upstream(); err == nil {
		t.Fatalf("Upstream() where git cannot answer = %q, nil; want git's error", upstream)
	}
}
