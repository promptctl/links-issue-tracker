package app

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

type App struct {
	Workspace workspace.Info
	Store     *store.Store
}

// AccessMode selects the store-open contract app construction uses: read
// requires an already-initialized database, write bootstraps one if absent.
// [LAW:one-source-of-truth] This is the only access-mode representation;
// callers (the CLI registration tables) carry these values, never their own.
type AccessMode string

const (
	AccessRead  AccessMode = "read"
	AccessWrite AccessMode = "write"
)

// Open is the single app construction path, parameterized by mode.
// [LAW:single-enforcer] Workspace resolution and store opening happen here
// only; there is no second factory to drift from this one.
func Open(ctx context.Context, cwd string, mode AccessMode) (*App, error) {
	ws, err := workspace.Resolve(cwd)
	if err != nil {
		return nil, err
	}
	st, err := mode.openStore(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return &App{Workspace: ws, Store: st}, nil
}

// openStore maps the mode value onto the store-open contract it names.
// [LAW:dataflow-not-control-flow] The read/write variance lives in this one
// value crossing one boundary, not in which factory a caller picked.
func (m AccessMode) openStore(ctx context.Context, databasePath, workspaceID string) (*store.Store, error) {
	switch m {
	case AccessRead:
		return store.OpenForRead(ctx, databasePath, workspaceID)
	case AccessWrite:
		return store.Open(ctx, databasePath, workspaceID)
	}
	// [LAW:no-silent-failure] An unknown mode (including the zero value) fails
	// closed instead of being granted write access by a default arm.
	return nil, fmt.Errorf("invalid access mode %q", string(m))
}

func (a *App) Close() error { return a.Store.Close() }
