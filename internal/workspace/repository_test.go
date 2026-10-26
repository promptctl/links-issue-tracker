package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// remotes is a populated remote — branch trunk, its HEAD naming trunk, and
// lit's refs/dolt/data — beside an empty one; dataRef is the line ls-remote
// prints for refs/dolt/data.
type remotes struct {
	populated, empty string
	dataRef          string
}

func newRemotes(t *testing.T) remotes {
	t.Helper()
	base := canonicalTempDir(t)
	source := filepath.Join(base, "source")
	r := remotes{populated: filepath.Join(base, "remote.git"), empty: filepath.Join(base, "empty.git")}
	run(t, base, "git", "init", "--bare", r.populated)
	run(t, base, "git", "init", "--bare", r.empty)
	run(t, base, "git", "init", source)
	run(t, source, "git", "checkout", "-b", "trunk")
	run(t, source, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	run(t, source, "git", "push", r.populated, "trunk", "HEAD:refs/dolt/data")
	run(t, base, "git", "--git-dir", r.populated, "symbolic-ref", "HEAD", "refs/heads/trunk")
	r.dataRef = runOutput(t, source, "git", "rev-parse", "HEAD") + "\trefs/dolt/data"
	return r
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// syncable is the remote named origin at url, an absolute path.
func syncable(t *testing.T, url string) SyncableRemote {
	t.Helper()
	remote, err := NewGitRemote("origin", url, CheckoutRoot{}).SyncableRemote()
	if err != nil {
		t.Fatalf("SyncableRemote(%q): %v", url, err)
	}
	return remote
}

// layout builds one of the repository layouts git produces and returns its
// common git dir, with origin configured to url and refs/remotes/origin/HEAD
// naming trunk. origin's upload-pack is a program that does not exist: a
// question asked by remote name rather than URL would fail on it.
type layout struct {
	name  string
	build func(t *testing.T, base string) string
}

var layouts = []layout{
	{"standard", func(t *testing.T, base string) string {
		run(t, base, "git", "init", "repo")
		return filepath.Join(base, "repo", ".git")
	}},
	{"bare", func(t *testing.T, base string) string {
		run(t, base, "git", "init", "--bare", "bare.git")
		return filepath.Join(base, "bare.git")
	}},
	{"separate git dir", func(t *testing.T, base string) string {
		run(t, base, "git", "init", "--separate-git-dir", filepath.Join(base, "sep.git"), "checkout")
		return filepath.Join(base, "sep.git")
	}},
	// A submodule's git dir carries core.worktree pointing at its checkout,
	// which is removed here: the question must not need it.
	{"submodule, checkout removed", func(t *testing.T, base string) string {
		sub := filepath.Join(base, "subsource")
		run(t, base, "git", "init", sub)
		run(t, sub, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
		run(t, base, "git", "init", "super")
		super := filepath.Join(base, "super")
		run(t, super, "git", "-c", "protocol.file.allow=always", "submodule", "add", sub, "sub")
		if err := os.RemoveAll(filepath.Join(super, "sub")); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(super, ".git", "modules", "sub")
	}},
}

func buildLayout(t *testing.T, l layout, url string) Repository {
	t.Helper()
	base := canonicalTempDir(t)
	gitDir := l.build(t, base)
	git := func(args ...string) {
		run(t, base, "git", append([]string{"--git-dir", gitDir, "--work-tree", gitDir}, args...)...)
	}
	git("config", "remote.origin.url", url)
	git("config", "remote.origin.uploadpack", filepath.Join(base, "no-such-upload-pack"))
	git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	return Location{GitCommonDir: gitDir}.Repository()
}

// environments are the ones a question must answer the same in: git config
// that refuses discovered bare repositories, and that config plus the
// repository-locating variables a git hook exports, each naming somewhere
// else.
func environments(t *testing.T) map[string]environ.Env {
	elsewhere := filepath.Join(canonicalTempDir(t), "elsewhere")
	explicit := testEnv().With("GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.bareRepository", "GIT_CONFIG_VALUE_0=explicit")
	return map[string]environ.Env{
		"safe.bareRepository=explicit": explicit,
		"under a git hook":             explicit.With("GIT_DIR="+elsewhere, "GIT_WORK_TREE="+elsewhere, "GIT_COMMON_DIR="+elsewhere),
	}
}

// Every question gets its answer in every layout, by URL and an explicit git
// dir: none discovers the repository from a directory, so none is refused by
// safe.bareRepository or misled by a hook's GIT_DIR, and none reads origin's
// name-keyed upload-pack.
func TestRepositoryQuestionsAnswerInEveryLayout(t *testing.T) {
	r := newRemotes(t)
	for envName, env := range environments(t) {
		for _, l := range layouts {
			t.Run(envName+"/"+l.name, func(t *testing.T) {
				repo := buildLayout(t, l, r.populated)
				ctx := context.Background()
				populated, empty := syncable(t, r.populated), syncable(t, r.empty)

				if has, err := repo.RemoteHasRefs(ctx, env, populated); err != nil || !has {
					t.Errorf("RemoteHasRefs(populated) = %v, %v; want true", has, err)
				}
				if has, err := repo.RemoteHasRefs(ctx, env, empty); err != nil || has {
					t.Errorf("RemoteHasRefs(empty) = %v, %v; want false", has, err)
				}
				if refs, err := repo.RemoteDoltRefs(ctx, env, populated); err != nil || refs != r.dataRef {
					t.Errorf("RemoteDoltRefs(populated) = %q, %v; want %q", refs, err, r.dataRef)
				}
				if has, err := repo.RemoteHasDoltData(ctx, env, empty); err != nil || has {
					t.Errorf("RemoteHasDoltData(empty) = %v, %v; want false", has, err)
				}
				if head, err := repo.AdvertisedRemoteHead(ctx, env, populated); err != nil || head != "trunk" {
					t.Errorf("AdvertisedRemoteHead(populated) = %q, %v; want trunk", head, err)
				}
				if head, err := repo.LocalRemoteHead(ctx, env, remoteName(t, "origin")); err != nil || head != "trunk" {
					t.Errorf("LocalRemoteHead(origin) = %q, %v; want trunk", head, err)
				}
				if head, err := repo.LocalRemoteHead(ctx, env, remoteName(t, "fork")); err != nil || head != "" {
					t.Errorf("LocalRemoteHead(fork) = %q, %v; want unset", head, err)
				}
			})
		}
	}
}

// An unset ref is "", nil; a git that cannot answer is an error, never unset,
// and so is a remote HEAD that names no branch of the remote.
// [LAW:no-silent-failure]
func TestLocalRemoteHeadTellsUnsetFromFailure(t *testing.T) {
	r := newRemotes(t)
	ctx := context.Background()

	repo := buildLayout(t, layouts[0], r.populated)
	run(t, repo.gitDir, "git", "--git-dir", repo.gitDir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/heads/trunk")
	if head, err := repo.LocalRemoteHead(ctx, testEnv(), remoteName(t, "origin")); err == nil {
		t.Errorf("LocalRemoteHead() = %q, nil for a HEAD naming refs/heads/trunk; want an error", head)
	}

	gone := buildLayout(t, layouts[1], r.populated)
	if err := os.RemoveAll(gone.gitDir); err != nil {
		t.Fatal(err)
	}
	if head, err := gone.LocalRemoteHead(ctx, testEnv(), remoteName(t, "origin")); err == nil {
		t.Errorf("LocalRemoteHead() = %q, nil in a removed git dir; want an error", head)
	}
}

// A local branch named origin/trunk changes what `symbolic-ref --short` prints
// for the remote's HEAD; the answer must not change with it.
func TestLocalRemoteHeadIgnoresALocalBranchOfTheSameName(t *testing.T) {
	r := newRemotes(t)
	repo := buildLayout(t, layouts[0], r.populated)
	checkout := filepath.Dir(repo.gitDir)
	run(t, checkout, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	run(t, checkout, "git", "update-ref", "refs/remotes/origin/trunk", "HEAD")
	run(t, checkout, "git", "branch", "origin/trunk")
	if head, err := repo.LocalRemoteHead(context.Background(), testEnv(), remoteName(t, "origin")); err != nil || head != "trunk" {
		t.Fatalf("LocalRemoteHead() = %q, %v; want trunk", head, err)
	}
}

// A URL that names no repository is an error, never "no refs".
func TestRemoteHasRefsFailsForAURLNamingNoRepository(t *testing.T) {
	repo := buildLayout(t, layouts[0], "unused")
	missing := syncable(t, filepath.Join(canonicalTempDir(t), "missing.git"))
	if has, err := repo.RemoteHasRefs(context.Background(), testEnv(), missing); err == nil {
		t.Fatalf("RemoteHasRefs() = %v, nil for a missing repository; want an error", has)
	}
}

// A URL that begins with "-" is a URL, not an option: written as an
// --upload-pack, it would run the command it names.
func TestRemoteQuestionsReadALeadingDashAsAURL(t *testing.T) {
	repo := buildLayout(t, layouts[0], "unused")
	remote := syncable(t, "--upload-pack=touch${IFS}PWNED;:x")
	if _, err := repo.RemoteHasRefs(context.Background(), testEnv(), remote); err == nil {
		t.Errorf("RemoteHasRefs() = nil error for an option-shaped URL; want git's refusal")
	}
	if _, err := os.Stat(filepath.Join(repo.gitDir, "PWNED")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("PWNED stat = %v; the URL ran as an --upload-pack", err)
	}
}

// A remote lit refuses is asked by its URL as written, read from the checkout
// root it was listed in, as git reads it there.
func TestCheckoutRemoteHasDoltDataReadsTheWrittenURLFromTheRoot(t *testing.T) {
	r := newRemotes(t)
	repo := buildLayout(t, layouts[0], r.populated)
	root := filepath.Dir(repo.gitDir)
	relative, err := filepath.Rel(root, r.populated)
	if err != nil {
		t.Fatal(err)
	}
	remote := NewGitRemote("origin", relative, testRoot(t, root))
	if has, err := repo.CheckoutRemoteHasDoltData(context.Background(), testEnv(), remote); err != nil || !has {
		t.Fatalf("CheckoutRemoteHasDoltData(%q) = %v, %v; want true", relative, has, err)
	}
}

func remoteName(t *testing.T, name string) storage.RemoteName {
	t.Helper()
	parsed, err := storage.ParseRemoteName(name)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
