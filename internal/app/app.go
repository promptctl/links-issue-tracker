package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

type App struct {
	Workspace workspace.Info
	Store     *store.Store
	// Stream is this checkout's opaque identity — the half of the attribution
	// pair that distinguishes two worktrees of one repository, whose other half
	// is Workspace.WorkspaceID. Under AccessWrite it is always present, minted
	// on the checkout's first mutating command; under AccessRead it is present
	// only if some earlier mutating command already minted it, and its absence
	// is the honest report that this checkout has produced no work evidence and
	// therefore holds no claim.
	Stream workspace.StreamID
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

// accessContract is everything an access mode decides, gathered into one value
// so the decisions cannot be made in different places and disagree. Store access
// and identity minting are the same question asked twice — "may this command
// change anything?" — and pairing them here means a mode physically cannot
// declare read-only store access while minting an identity, and a mode added
// later cannot supply one behavior and silently inherit a default for the other.
// [LAW:one-type-per-behavior] The modes differ only in these two values, so they
// are two instances of one type, not two code paths.
type accessContract struct {
	openStore     func(ctx context.Context, databasePath string, workspaceID string) (*store.Store, error)
	resolveStream func(privateGitDir string) (workspace.StreamID, error)
}

// accessContracts is the whole meaning of AccessMode. Read commands open the
// store read-only and READ an existing identity without creating one; write
// commands bootstrap the store and ENSURE an identity exists. The right-hand
// column is the mechanism behind "a checkout that has only ever been read never
// mints a token" — it is a property of this table, not of discipline at call
// sites. [LAW:dataflow-not-control-flow] The variance is data in this map, not
// branches in Open.
var accessContracts = map[AccessMode]accessContract{
	AccessRead:  {openStore: store.OpenForRead, resolveStream: workspace.ReadStream},
	AccessWrite: {openStore: store.Open, resolveStream: workspace.EnsureStream},
}

// Open is the single app construction path, parameterized by mode.
// [LAW:single-enforcer] Workspace resolution, store opening, and identity
// resolution happen here only; there is no second factory to drift from this
// one, and the mode is validated exactly once — the lookup below is both the
// validity check and the dispatch, so no second switch can classify an unknown
// mode differently.
func Open(ctx context.Context, cwd string, mode AccessMode) (*App, error) {
	contract, known := accessContracts[mode]
	if !known {
		// [LAW:no-silent-failure] An unknown mode (including the zero value)
		// fails closed instead of being granted write access by a default arm.
		return nil, fmt.Errorf("invalid access mode %q", string(mode))
	}
	ws, err := workspace.Resolve(cwd)
	if err != nil {
		return nil, err
	}
	st, err := contract.openStore(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return nil, err
	}
	// Resolved after the store opens so a command that cannot reach its store
	// mints nothing: identity marks a checkout that started doing work, and a
	// failure to open is not that.
	stream, err := contract.resolveStream(ws.PrivateGitDir)
	if err != nil {
		// The store must be closed because no App is returned to close it, and
		// Store.Close is also what releases the workspace lock — a release that
		// fails here strands the lock and resurfaces later as an unexplained
		// "workspace busy". Joined rather than discarded so the identity failure
		// and a stranded lock are both visible; this is the close-on-abort
		// pattern internal/store/store.go already uses for the same reason.
		// [LAW:no-silent-failure]
		return nil, errors.Join(err, st.Close())
	}
	return &App{Workspace: ws, Store: st, Stream: stream}, nil
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
