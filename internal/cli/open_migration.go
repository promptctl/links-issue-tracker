package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// reportOpenMigration is what a command does with what opening a store did to
// it: it records the facts on the command's event, and writes the warnings the
// failures the open survived leave, none when nothing failed. The open has
// done its work either way, so the command goes on to run.
//
// The warnings go to stderr, as the statusline's warnings do: they are about
// the store, not the command's result, and stdout may be an export or a --json
// document that a leading line would corrupt. A failed write has no channel
// left to report on and cannot change what the command did, so it changes
// nothing. [LAW:dataflow-not-control-flow] the warnings are values written
// every time.
func reportOpenMigration(ctx context.Context, done storage.Migrated) {
	telemetry.Record(ctx, done.EventRecord(), nil)
	fmt.Fprint(os.Stderr, strings.Join(openWarnings(done), ""))
}

// openWarnings is the lines the failures an open survived add to a command's
// output, one per failure, each ending in a newline.
func openWarnings(done storage.Migrated) []string {
	var warnings []string
	for _, failure := range done.Failures() {
		warnings = append(warnings, fmt.Sprintf("warning: %v\n", failure))
	}
	return warnings
}
