package cli

import (
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dirtemplate"
	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// storeTemplate is the migrated store newTestCLIApp's workspaces start from,
// built once per test binary by the very open the helper used to run for every
// test: the read-write engine open, under the same workspace id. A copy is
// therefore the database that open would have built, down to the author of its
// bootstrap and migration commits. [LAW:one-source-of-truth]
var storeTemplate = dirtemplate.New("dolt", func(dir string) error {
	st, err := engine.Open(context.Background(), engine.ReadWrite, dir, "test-workspace-id")
	if err != nil {
		return err
	}
	return st.Close()
})

// migratedDoltDir is a fresh, private copy of storeTemplate inside
// t.TempDir(), named dolt as a workspace's database directory is.
//
// Only a helper that opens one store directly takes it. Tests that create a
// workspace through `lit init` or a first write in a git repository keep
// building from nothing: a store already in place makes init report the
// workspace as already initialized, and two checkouts seeded from one template
// share history that production checkouts never would.
func migratedDoltDir(t testing.TB) string {
	t.Helper()
	return storeTemplate.Copy(t)
}

// The fixture's whole saving: a copy is already the store this binary's open
// would build, so opening it neither builds a database nor applies a migration.
// A head read before the open proves the database was already there, and a
// migration always commits, so the same head after it proves none ran.
func TestAMigratedDoltDirOpensWithoutBuildingOrMigrating(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{DatabasePath: migratedDoltDir(t)}, WorkspaceID: "test-workspace-id"}
	before := doltHead(t, ws)

	st, err := engine.Open(context.Background(), engine.ReadWrite, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if after := doltHead(t, ws); after != before {
		t.Fatalf("opening a copy moved HEAD from %s to %s: it migrated", before, after)
	}
}
