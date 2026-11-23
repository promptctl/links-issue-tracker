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

// WorkspaceID is the workspace id a built workspace starts with, and the one a
// test opens a store under when that store must belong to it.
const WorkspaceID = "test-workspace-id"

// New, StoreAt and Handed build a workspace.Info, so a test names only the
// fields its assertion depends on, and a field added to workspace.Info is given
// its default here and in no test file. [LAW:one-source-of-truth]
// [LAW:composability]
//
// Each is a shape production has, its geometry whole: New and StoreAt a store
// with no checkout, Handed the root and store a detached worker is handed.
// [LAW:types-are-the-program] Each starts with the workspace id WorkspaceID,
// the issue prefix "test", and the project config handed as a value that
// configures nothing, which the With functions change. A real checkout's
// workspace is Resolved, whose identity and config are the ones on disk.

// New is a workspace of t's own with no checkout: a store geometry under a
// fresh temp directory, whose storage directory exists and whose store has not
// been created. env is the environment its remotes ask git under, and they
// fail, as no repository is there.
func New(t testing.TB, env environ.Env) workspace.Info {
	t.Helper()
	ws := StoreAt(t, env, workspace.LocationFromGitCommonDir(canonical(t, t.TempDir())).StorageDir)
	if err := os.MkdirAll(ws.StorageDir, 0o755); err != nil {
		t.Fatalf("make the workspace's storage dir: %v", err)
	}
	return ws
}

// StoreAt is a workspace with no checkout whose store is the one found at
// storageDir, its whole geometry derived from it. Nothing is created:
// storageDir is the store as the test found it, which may be one that does not
// exist or cannot.
func StoreAt(t testing.TB, env environ.Env, storageDir string) workspace.Info {
	t.Helper()
	loc := workspace.LocationFromStorageDir(storageDir)
	return workspace.Info{
		Location:    loc,
		WorkspaceID: WorkspaceID,
		IssuePrefix: prefixState(t, "test"),
		Remotes:     workspace.NewRemotes(env, loc.Repository(), workspace.CheckoutRoot{}),
		Project:     configtest.HandedProject(""),
	}
}

// Handed is the workspace a detached worker is handed for the checkout at
// root, as workspace.Handed builds it: New's store, the root that relative
// paths are joined to, and no sync remote. Nothing is asked of git about the
// checkout, so root need not be a repository.
func Handed(t testing.TB, env environ.Env, root string) workspace.Info {
	t.Helper()
	checkout, err := workspace.ParseCheckoutRoot(canonical(t, root))
	if err != nil {
		t.Fatalf("checkout root %q: %v", root, err)
	}
	ws := New(t, env)
	ws.RootDir = checkout.String()
	ws.Remotes = workspace.HandedRemotes(env, ws.Location.Repository(), checkout, "")
	return ws
}

// Resolved is the workspace a command run in the repository at dir resolves:
// workspace.Resolve itself, identity and project config included.
func Resolved(t testing.TB, env environ.Env, dir string) workspace.Info {
	t.Helper()
	ws, err := workspace.Resolve(env, dir)
	if err != nil {
		t.Fatalf("resolve the workspace at %s: %v", dir, err)
	}
	return ws
}

// WithWorkspaceID is ws with the workspace id id.
func WithWorkspaceID(ws workspace.Info, id string) workspace.Info {
	ws.WorkspaceID = id
	return ws
}

// WithIssuePrefix is ws minting under the legal prefix raw.
func WithIssuePrefix(t testing.TB, ws workspace.Info, raw string) workspace.Info {
	t.Helper()
	ws.IssuePrefix = prefixState(t, raw)
	return ws
}

// WithProject is ws running under the handed project config toml.
func WithProject(ws workspace.Info, toml string) workspace.Info {
	ws.Project = configtest.HandedProject(toml)
	return ws
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
