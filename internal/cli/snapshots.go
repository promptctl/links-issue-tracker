package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

var snapshotsFamily = commandFamily[wsRunFn]{
	usage: "usage: lit snapshots <new|list|restore> ...",
	subcommands: []subcommandRow[wsRunFn]{
		{name: "new", payload: runSnapshotsNew},
		{name: "list", payload: func(_ context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
			return runSnapshotsList(stdout, ws, args)
		}},
		{name: "restore", payload: runSnapshotsRestore},
	},
}

// snapshotsDirFor returns the workspace's filesystem-snapshot directory.
// [LAW:one-source-of-truth] All snapshot-path construction flows through this
// helper; callers don't compose <storageDir>/snapshots themselves.
func snapshotsDirFor(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "snapshots")
}

// isUserSnapshotName reports whether name is a user snapshot (i.e. produced
// by `lit snapshots new`). It excludes every system-stamped kind so each
// producer's retention budget governs only its own snapshots — the user
// budget cannot collect a migration recovery point or a downgrade recovery
// point.
//
// [LAW:one-source-of-truth] Each system producer owns its own kind predicate
// (store.IsMigrationSnapshotName, store.IsDowngradeSnapshotName); this helper
// composes those — adding a new producer means adding the predicate to this
// disjunction, in exactly one place.
func isUserSnapshotName(name string) bool {
	return !store.IsMigrationSnapshotName(name) && !store.IsDowngradeSnapshotName(name)
}

// withCommitLock acquires the path-based commit lock used by Store mutations
// so a clone/restore can't interleave with concurrent writes from `lit update`
// or any other in-process mutation. Routes through store.LockCommitPath so the
// lock primitive stays single-source.
//
// Reader-vs-restore exclusion is owned by the workspace-busy lock acquired in
// store.Open / store.OpenForRead (shared) and by runSnapshotsRestore
// (exclusive); this commit lock remains the writer-vs-writer gate only.
func withCommitLock(ctx context.Context, ws workspace.Info, fn func() error) error {
	release, err := store.LockCommitPath(ctx, store.CommitLockPath(ws.DatabasePath))
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

func runSnapshotsNew(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("snapshots new")
	label := fs.String("label", "", "Optional human-readable label appended to the snapshot name")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	cfg, err := config.Load(pathspec.New(ws.RootDir))
	if err != nil {
		return err
	}
	var snap dbsnapshot.Snapshot
	if err := withCommitLock(ctx, ws, func() error {
		s, err := dbsnapshot.Take(ws.DatabasePath, snapshotsDirFor(ws), strings.TrimSpace(*label))
		if err != nil {
			return err
		}
		snap = s
		// [LAW:single-enforcer] User-snapshot retention bounds *user*
		// snapshots only; migration snapshots share the directory but are
		// pruned independently by migrate() under its own budget. Without
		// the kind filter, `lit snapshots new` could evict a recovery
		// snapshot the migration system is depending on.
		return dbsnapshot.PruneMatching(snapshotsDirFor(ws), cfg.Snapshot.RetentionBudget, isUserSnapshotName)
	}); err != nil {
		return err
	}
	return printValue(stdout, snap, *jsonOut, func(w io.Writer, v any) error {
		s := v.(dbsnapshot.Snapshot)
		_, err := fmt.Fprintf(w, "%s %s\n", s.Name, s.Path)
		return err
	})
}

func runSnapshotsList(stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("snapshots list")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	snapshots, err := dbsnapshot.List(snapshotsDirFor(ws))
	if err != nil {
		return err
	}
	return printValue(stdout, snapshots, *jsonOut, func(w io.Writer, v any) error {
		list := v.([]dbsnapshot.Snapshot)
		for _, snap := range list {
			if _, err := fmt.Fprintf(w, "%s %s %s\n", snap.Name, snap.Created.Format("2006-01-02T15:04:05Z"), snap.Path); err != nil {
				return err
			}
		}
		return nil
	})
}

func runSnapshotsRestore(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) (err error) {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("snapshots restore")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 || fs.NArg() != 0 {
		return errors.New("usage: lit snapshots restore <name> [--json]")
	}
	name := strings.TrimSpace(positional[0])
	if name == "" {
		return errors.New("usage: lit snapshots restore <name> [--json]")
	}
	// [LAW:single-enforcer] Exclusive workspace lock owns reader-vs-restore
	// exclusion; commit lock (held inside withCommitLock below) owns
	// writer-vs-restore exclusion. Both held while the Dolt directory is
	// rotated so no Store — open or about to open — can observe the rename.
	releaseWorkspace, err := store.LockWorkspaceExclusive(ctx, ws.DatabasePath)
	if err != nil {
		return err
	}
	// [LAW:no-silent-fallbacks] A release failure is rare but real (e.g.
	// EBADF on a torn FD) and signals workspace-lock state the operator
	// needs to know about; surface it via the named return rather than
	// discarding. errors.Join keeps both observable — a release failure
	// matters whether or not the restore itself succeeded, because either
	// way it can leave the workspace stuck busy for subsequent commands.
	defer func() {
		if relErr := releaseWorkspace(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	var rotated string
	if err := withCommitLock(ctx, ws, func() error {
		r, err := dbsnapshot.Restore(ws.DatabasePath, snapshotsDirFor(ws), name)
		if err != nil {
			return err
		}
		rotated = r
		return nil
	}); err != nil {
		return err
	}
	payload := map[string]string{
		"status":     "restored",
		"name":       name,
		"database":   ws.DatabasePath,
		"rotated_to": rotated,
	}
	return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
		p := v.(map[string]string)
		if p["rotated_to"] == "" {
			_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["name"])
			return err
		}
		_, err := fmt.Fprintf(w, "%s %s rotated_to=%s\n", p["status"], p["name"], p["rotated_to"])
		return err
	})
}
