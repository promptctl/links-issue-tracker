// Package workspacetest holds the one builder through which a test in any
// package gets the workspace.Info it runs against.
package workspacetest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/config/configtest"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// WorkspaceID is the workspace id every built workspace starts with, and the
// one a test opens a store under when that store must belong to it.
const WorkspaceID = "test-workspace-id"

// Workspace builds a workspace.Info, so a test names only the fields its
// assertion depends on, and a field added to workspace.Info is given its
// default here and in no test file. [LAW:one-source-of-truth]
// [LAW:composability]
//
// Its geometry is fixed, whole, by the construction that made it, and each
// construction is a shape production has: New and StoreAt a store with no
// checkout, Handed the root and store a detached worker is handed, Checkout
// the workspace Resolve gives a real repository. No method changes it.
// [LAW:types-are-the-program] Whatever the construction, the workspace id is
// WorkspaceID, the issue prefix "test", and the project config handed as a
// value that configures nothing, so a test that needs others names them.
//
// Each method returns a new Workspace and leaves its receiver as it was, so a
// helper can hand back a partly built one for each caller to finish its own
// way.
type Workspace struct {
	t    testing.TB
	info workspace.Info
}

// New is a workspace of t's own with no checkout: a store geometry under a
// fresh temp directory, whose storage directory exists and whose store has not
// been created. env is the environment its remotes ask git under, and they
// fail, as no repository is there.
func New(t testing.TB, env environ.Env) Workspace {
	t.Helper()
	w := StoreAt(t, env, workspace.LocationFromGitCommonDir(canonical(t, t.TempDir())).StorageDir)
	if err := os.MkdirAll(w.info.StorageDir, 0o755); err != nil {
		t.Fatalf("make the workspace's storage dir: %v", err)
	}
	return w
}

// StoreAt is a workspace with no checkout whose store is the one found at
// storageDir, its whole geometry derived from it. Nothing is created:
// storageDir is the store as the test found it, which may be one that does not
// exist or cannot.
func StoreAt(t testing.TB, env environ.Env, storageDir string) Workspace {
	t.Helper()
	loc := workspace.LocationFromStorageDir(storageDir)
	return built(t, workspace.Info{
		Location: loc,
		Remotes:  workspace.NewRemotes(env, loc.Repository(), workspace.CheckoutRoot{}),
	})
}

// Handed is the workspace a detached worker is handed for the checkout at
// root, as workspace.Handed builds it: New's store, the root that relative
// paths are joined to, and no sync remote. Nothing is asked of git about the
// checkout, so root need not be a repository.
func Handed(t testing.TB, env environ.Env, root string) Workspace {
	t.Helper()
	checkout, err := workspace.ParseCheckoutRoot(canonical(t, root))
	if err != nil {
		t.Fatalf("checkout root %q: %v", root, err)
	}
	w := New(t, env)
	w.info.RootDir = checkout.String()
	w.info.Remotes = workspace.HandedRemotes(env, w.info.Location.Repository(), checkout, "")
	return w
}

// Checkout is the workspace a command run in the repository at dir works on:
// workspace.Resolve's own geometry, root and remotes, so dir must be a
// repository Resolve accepts.
func Checkout(t testing.TB, env environ.Env, dir string) Workspace {
	t.Helper()
	resolved := Resolved(t, env, dir)
	return built(t, workspace.Info{
		Location:      resolved.Location,
		RootDir:       resolved.RootDir,
		PrivateGitDir: resolved.PrivateGitDir,
		Remotes:       resolved.Remotes,
	})
}

// WorkspaceID is w with the workspace id id.
func (w Workspace) WorkspaceID(id string) Workspace {
	w.info.WorkspaceID = id
	return w
}

// IssuePrefix is w minting under the legal prefix raw.
func (w Workspace) IssuePrefix(raw string) Workspace {
	w.t.Helper()
	w.info.IssuePrefix = prefixState(w.t, raw)
	return w
}

// WithProject is ws running under the handed project config toml, for a test
// that holds a workspace already, from the builder, an app or a helper, and
// changes the config its later commands run under.
func WithProject(ws workspace.Info, toml string) workspace.Info {
	ws.Project = configtest.HandedProject(toml)
	return ws
}

// Info is the workspace w describes.
func (w Workspace) Info() workspace.Info { return w.info }

// Resolved is the workspace a command run in dir resolves: workspace.Resolve
// itself, identity and project config included, for a test where lit has
// initialized dir, or whose subject is a real checkout's own config, identity
// or geometry.
func Resolved(t testing.TB, env environ.Env, dir string) workspace.Info {
	t.Helper()
	ws, err := workspace.Resolve(env, dir)
	if err != nil {
		t.Fatalf("resolve the workspace at %s: %v", dir, err)
	}
	return ws
}

// built is a workspace of geometry, with the values every construction shares.
func built(t testing.TB, geometry workspace.Info) Workspace {
	t.Helper()
	geometry.WorkspaceID = WorkspaceID
	geometry.IssuePrefix = prefixState(t, "test")
	geometry.Project = configtest.HandedProject("")
	return Workspace{t: t, info: geometry}
}

// canonical is dir with its symlinks resolved, the physical path Resolve
// derives every store path from.
func canonical(t testing.TB, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %q: %v", dir, err)
	}
	return resolved
}

func prefixState(t testing.TB, raw string) workspace.PrefixState {
	t.Helper()
	spec, err := workspace.ConfiguredPrefix(raw)
	if err != nil {
		t.Fatalf("ConfiguredPrefix(%q): %v", raw, err)
	}
	return workspace.LegalPrefixState(spec)
}
