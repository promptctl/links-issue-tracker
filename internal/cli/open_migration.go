package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// reportOpenMigration is what a command does with a store it has just opened:
// it records what the open did to the stored shape on the command's event, and
// writes the warning a failed snapshot prune leaves, "" when nothing failed.
// The migration has committed either way, so the command goes on to run.
// [LAW:dataflow-not-control-flow] the warning is a value written every time.
func reportOpenMigration(ctx context.Context, w io.Writer, st storage.Store) error {
	done := openMigration(st)
	telemetry.Record(ctx, done.EventRecord(), nil)
	_, err := io.WriteString(w, migrationPruneWarning(done.PruneErr))
	return err
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
