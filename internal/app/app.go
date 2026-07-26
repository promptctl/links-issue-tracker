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

// OpenLocationForRead opens the store at an already-derived Location strictly
// read-only, bypassing the current working directory's git resolution entirely:
// the caller supplies the store's geometry as a value (from workspace.Discover or
// workspace.LocationFromStorageDir), so this opens a store the process is not
// cd'd into. It is the cross-project open primitive — aggregation over many
// stores opens each one through here.
//
// [LAW:single-enforcer] Store opening still routes through store.OpenForRead, the
// one read path, so a foreign store gets exactly the shared lock and read
// contract a local read gets — never a second read-write engine that the embedded
// Dolt driver would reject as "database is read only" and that would contend with
// the project's own writer.
//
// The workspace_id OpenForRead requires is not part of a Location — discovery
// reads no config — so it is read here from the store's own config.json via
// ReadConfig, a pure read that never writes the foreign store.
func OpenLocationForRead(ctx context.Context, loc workspace.Location) (*store.Store, error) {
	cfg, err := workspace.ReadConfig(loc.ConfigPath)
	if err != nil {
		return nil, err
	}
	return store.OpenForRead(ctx, loc.DatabasePath, cfg.WorkspaceID)
}

func (a *App) Close() error { return a.Store.Close() }
