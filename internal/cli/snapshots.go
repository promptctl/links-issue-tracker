package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

// statSnapshotDoltDir probes a snapshot directory for the expected Dolt
// repo layout (the `links/.dolt` subdirectory the embedded server creates).
// Returns the FileInfo on success. Used by restore to refuse pointing
// at a path that obviously isn't a Dolt repo backup.
//
// The DatabasePath that's passed everywhere is the dolt root (e.g.,
// .git/links/dolt), so the snapshot contents mirror that root and the
// embedded database lives one level deeper at `<snapshot>/links/.dolt`.
func statSnapshotDoltDir(snapshotPath string) (os.FileInfo, error) {
	return os.Stat(filepath.Join(snapshotPath, "links", ".dolt"))
}

// validateSnapshotsCommandPath gates the `lit snapshots` command tree.
// Subcommands are: list, restore.
func validateSnapshotsCommandPath(args []string) error {
	return validateNestedCommandPath(args,
		"usage: lit snapshots <list|restore> [args]", "list", "restore")
}

// runSnapshots dispatches the `lit snapshots` subcommand. Crucially, this
// path does NOT open the Dolt SQL connection — opening would itself take a
// snapshot (defeating the read-only `list`) and would hold a lock that
// blocks `restore`'s directory rename.
func runSnapshots(_ context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit snapshots <list|restore> [args]")
	}
	switch args[0] {
	case "list":
		return runSnapshotsList(stdout, ws, args[1:])
	case "restore":
		return runSnapshotsRestore(stdout, ws, args[1:])
	default:
		return fmt.Errorf("unknown snapshots subcommand %q (expected list or restore)", args[0])
	}
}

func runSnapshotsList(stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("snapshots list")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}

	snaps, err := store.ListWorkspaceSnapshots(ws.DatabasePath)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}

	if *jsonOut {
		return printValue(stdout, snaps, true, nil)
	}
	if len(snaps) == 0 {
		_, err := fmt.Fprintln(stdout, "No snapshots found.")
		return err
	}
	// Plain-text rendering: one line per snapshot, newest first.
	// Fields are name, age (rounded), path. Keep it parseable with `awk`.
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].CreatedAt.After(snaps[j].CreatedAt) })
	for _, s := range snaps {
		if _, err := fmt.Fprintf(stdout, "%s\tcreated=%s\tpath=%s\n",
			s.Name, s.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), s.Path); err != nil {
			return err
		}
	}
	return nil
}

func runSnapshotsRestore(stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("snapshots restore")
	name := fs.String("name", "", "Snapshot name (run `lit snapshots list` to see names)")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	chosen := strings.TrimSpace(*name)
	if chosen == "" && fs.cmd.Flags().NArg() > 0 {
		chosen = fs.cmd.Flags().Arg(0)
	}
	if chosen == "" {
		return errors.New("usage: lit snapshots restore <name>")
	}

	// Probe up-front: the snapshot must exist, and it must look like a
	// Dolt repo (contain the embedded `links/.dolt` subdirectory). Failing
	// here is cheap and prevents wiping a healthy workspace because of a
	// typo'd snapshot name.
	snapsDir := store.SnapshotsDirForDoltRepo(ws.DatabasePath)
	if _, err := store.LookupWorkspaceSnapshot(snapsDir, chosen); err != nil {
		return err
	}
	if _, err := statSnapshotDoltDir(filepath.Join(snapsDir, chosen)); err != nil {
		return fmt.Errorf("snapshot %q does not look like a Dolt repo: %w", chosen, err)
	}

	result, err := store.RestoreWorkspaceSnapshot(snapsDir, ws.DatabasePath, chosen)
	if err != nil {
		return fmt.Errorf("restore snapshot %q: %w", chosen, err)
	}

	if *jsonOut {
		return printValue(stdout, result, true, nil)
	}
	_, err = fmt.Fprintf(stdout,
		"Restored %s into %s. Previous Dolt repo moved aside to %s — delete when you're confident the restore is correct.\n",
		result.Name, result.DoltRepoPath, result.PreRestorePath)
	return err
}
