package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// reportOpenMigration is what a command does with what opening a store did to
// its stored shape: it records the facts on the command's event, and writes
// the warning a failed snapshot prune leaves, "" when nothing failed. The
// migration has committed either way, so the command goes on to run.
//
// The warning goes to stderr, as the statusline's warnings do: it is about the
// store, not the command's result, and stdout may be an export or a --json
// document that a leading line would corrupt. A failed write has no channel
// left to report on and cannot change what the command did, so it changes
// nothing. [LAW:dataflow-not-control-flow] the warning is a value written
// every time.
func reportOpenMigration(ctx context.Context, done storage.Migrated) {
	telemetry.Record(ctx, done.EventRecord(), nil)
	fmt.Fprint(os.Stderr, migrationPruneWarning(done.PruneErr))
}

// openMigration is what opening st did to its stored shape. An engine whose
// data has no versioned shape has nothing to migrate, so it migrated nothing.
func openMigration(st storage.Store) storage.Migrated {
	migrator, err := storage.SchemaMigration.Of(st)
	if err != nil {
		return storage.Migrated{}
	}
	return migrator.Migrated()
}

// migrationPruneWarning is the line an open's migration adds to the command's
// output when rolling old migration snapshots off failed after the migration
// committed, or "" when nothing failed.
func migrationPruneWarning(pruneErr error) string {
	if pruneErr == nil {
		return ""
	}
	return fmt.Sprintf("warning: old migration snapshots were not removed: %v\n", pruneErr)
}
