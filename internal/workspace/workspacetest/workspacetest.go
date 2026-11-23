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
// Its geometry is derived as workspace.Resolve derives it, which this
// package's tests check against Resolve itself. Its project config is handed
// as a value rather than read from the checkout, and its issue prefix is
// "test" rather than one derived from a repository name.
//
// Each method returns a new Workspace and leaves its receiver as it was, so a
// helper can hand back a partly built one for each caller to finish its own
// way.
type Workspace struct {
	t    testing.TB
	env  environ.Env
	info workspace.Info
	// root is the checkout the workspace runs from. Its zero value is no
	// checkout, whose remote questions fail, so Info always builds the
	// remotes the same way. [LAW:dataflow-not-control-flow]
	root workspace.CheckoutRoot
}

// New is a workspace of t's own with no checkout: a store geometry under a
// fresh temp directory, whose storage directory exists and whose store has not
// been created, the workspace id WorkspaceID, issue prefix "test", and a
// project config that configures nothing. env is the environment its remotes
// ask git under.
func New(t testing.TB, env environ.Env) Workspace {
	t.Helper()
	w := Workspace{t: t, env: env, info: workspace.Info{
		Location:    workspace.LocationFromGitCommonDir(t.TempDir()),
		WorkspaceID: WorkspaceID,
		IssuePrefix: prefixState(t, "test"),
		Project:     configtest.HandedProject(""),
	}}
	w.makeStorageDir()
	return w
}

// Checkout is w as run from the primary clone at dir, whose git directory is
// dir/.git: its root, its store where Resolve puts that clone's store, whose
// storage directory exists, and the remotes git reports there. dir's
// symlinks are resolved, as git resolves --show-toplevel and
// --git-common-dir. Nothing is asked of git until a test asks the remotes, so
// dir need not be a repository. A linked worktree's geometry is not a primary
// clone's; a test that needs one gets it from Resolved.
func (w Workspace) Checkout(dir string) Workspace {
	w.t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		w.t.Fatalf("resolve checkout %q: %v", dir, err)
	}
	root, err := workspace.ParseCheckoutRoot(resolved)
	if err != nil {
		w.t.Fatalf("checkout root %q: %v", resolved, err)
	}
	gitDir := filepath.Join(resolved, ".git")
	w.root = root
	w.info.Location = workspace.LocationFromGitCommonDir(gitDir)
	w.info.PrivateGitDir = gitDir
	w.makeStorageDir()
	return w
}

// StorageDir is w with its store's whole geometry derived from dir, as a
// store found at dir has it. Nothing is created: dir is the store as the test
// found it, which may be one that does not exist or cannot.
func (w Workspace) StorageDir(dir string) Workspace {
	w.info.Location = workspace.LocationFromStorageDir(dir)
	return w
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

// Info is the workspace w describes. Its root and remotes are derived from
// its checkout and its store's repository on every call, so no override can
// leave them describing another. [LAW:one-source-of-truth]
func (w Workspace) Info() workspace.Info {
	info := w.info
	info.RootDir = w.root.String()
	info.Remotes = workspace.NewRemotes(w.env, info.Location.Repository(), w.root)
	return info
}

// Resolved is the workspace a command run in dir resolves: workspace.Resolve
// itself, for a test whose subject is resolution or a real checkout's own
// config, identity or geometry.
func Resolved(t testing.TB, env environ.Env, dir string) workspace.Info {
	t.Helper()
	ws, err := workspace.Resolve(env, dir)
	if err != nil {
		t.Fatalf("resolve the workspace at %s: %v", dir, err)
	}
	return ws
}

// makeStorageDir creates w's storage directory, as Resolve does before any
// command reads it.
func (w Workspace) makeStorageDir() {
	w.t.Helper()
	if err := os.MkdirAll(w.info.StorageDir, 0o755); err != nil {
		w.t.Fatalf("make the workspace's storage dir: %v", err)
	}
}

func prefixState(t testing.TB, raw string) workspace.PrefixState {
	t.Helper()
	spec, err := workspace.ConfiguredPrefix(raw)
	if err != nil {
		t.Fatalf("ConfiguredPrefix(%q): %v", raw, err)
	}
	return workspace.LegalPrefixState(spec)
}
