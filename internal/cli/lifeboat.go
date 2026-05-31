package cli

import (
	"context"
	"errors"
	"io"

	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

const lifeboatUsage = "usage: lit lifeboat <dump> ..."

func validateLifeboatCommandPath(args []string) error {
	return validateNestedCommandPath(args, lifeboatUsage, "dump")
}

// runLifeboat is the data lifeboat command surface (links-recovery-j0vl): the
// recovery path that reads a workspace's data below the migration gate, so a
// workspace store.Open() refuses can still be released and rebuilt. This
// foundation registers the surface and its first verb, `dump`; later verbs in
// the epic (map/apply/verify/run) attach here.
func runLifeboat(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New(lifeboatUsage)
	}
	switch args[0] {
	case "dump":
		return runLifeboatDump(ctx, stdout, ws, args[1:])
	default:
		return errors.New(lifeboatUsage)
	}
}

// runLifeboatDump emits the workspace's complete raw contents as a portable
// JSON artifact, read below the migration gate. Like `lit export`, it is
// JSON-only: there is no meaningful text rendering of a full database dump, and
// the artifact is consumed by tools, not read by hand.
func runLifeboatDump(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("lifeboat dump")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit lifeboat dump")
	}
	dump, err := store.DumpRaw(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return err
	}
	return writeJSON(stdout, dump)
}
