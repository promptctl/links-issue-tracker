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

// Workspace builds a workspace.Info. Every field starts at what a resolved
// workspace holds, so a test names only the fields its assertion depends on,
// and a field added to workspace.Info is given its default here and in no
// test file. [LAW:one-source-of-truth] [LAW:composability]
//
// Each method returns a new Workspace and leaves its receiver as it was, so a
// helper can hand back a partly built one for each caller to finish its own
// way.
type Workspace struct {
	t    testing.TB
	env  environ.Env
	info workspace.Info
	// root and repo are the checkout the workspace's remotes are asked of.
	// Their zero values are no checkout, whose questions fail, so Info always
	// builds the remotes the same way. [LAW:dataflow-not-control-flow]
	root workspace.CheckoutRoot
	repo workspace.Repository
}

// New is a workspace of t's own with no checkout: a store geometry under a
// fresh temp directory, whose storage directory exists and whose store has not
// been created, the workspace id the CLI's store template is built under,
// issue prefix "test", and a project config that configures nothing. env is
// the environment its remotes ask git under.
func New(t testing.TB, env environ.Env) Workspace {
	t.Helper()
	loc := workspace.LocationFromStorageDir(filepath.Join(t.TempDir(), "links"))
	if err := os.MkdirAll(loc.StorageDir, 0o755); err != nil {
		t.Fatalf("make the workspace's storage dir: %v", err)
	}
	return Workspace{t: t, env: env, info: workspace.Info{
		Location:    loc,
		WorkspaceID: "test-workspace-id",
		IssuePrefix: prefixState(t, "test"),
		Project:     configtest.Project(""),
	}}
}

// Checkout is w as run from the checkout at dir, whose repository is
// dir/.git: its root, and the remotes git reports there. dir's symlinks are
// resolved, as git resolves --show-toplevel. Nothing is asked of git until a
// test asks the remotes, so dir need not be a repository.
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
	w.root = root
	w.repo = workspace.LocationFromStorageDir(filepath.Join(resolved, ".git", "links")).Repository()
	w.info.RootDir = resolved
	return w
}

// Location is w with its store at loc.
func (w Workspace) Location(loc workspace.Location) Workspace {
	w.info.Location = loc
	return w
}

// StorageDir is w with its store's whole geometry derived from dir, as a
// store found at dir has it.
func (w Workspace) StorageDir(dir string) Workspace {
	return w.Location(workspace.LocationFromStorageDir(dir))
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

// Project is w running under the project config toml.
func (w Workspace) Project(toml string) Workspace {
	w.info = UnderProject(w.info, toml)
	return w
}

// UnderProject is ws running under the project config toml, for a test that
// holds a workspace already, inside an app or from a helper, and changes the
// config its later commands run under.
func UnderProject(ws workspace.Info, toml string) workspace.Info {
	ws.Project = configtest.Project(toml)
	return ws
}

// Info is the workspace w describes.
func (w Workspace) Info() workspace.Info {
	info := w.info
	info.Remotes = workspace.NewRemotes(w.env, w.repo, w.root)
	return info
}

func prefixState(t testing.TB, raw string) workspace.PrefixState {
	t.Helper()
	spec, err := workspace.ConfiguredPrefix(raw)
	if err != nil {
		t.Fatalf("ConfiguredPrefix(%q): %v", raw, err)
	}
	return workspace.LegalPrefixState(spec)
}
