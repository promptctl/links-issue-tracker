package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/config"
	"github.com/bmf/links-issue-tracker/internal/dbsnapshot"
	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func validateSnapshotsCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit snapshots <new|list|restore> ...", "new", "list", "restore")
}

// snapshotsDirFor returns the workspace's filesystem-snapshot directory.
// [LAW:one-source-of-truth] All snapshot-path construction flows through this
// helper; callers don't compose <storageDir>/snapshots themselves.
func snapshotsDirFor(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "snapshots")
}

// isUserSnapshotName reports whether name is a user snapshot (i.e. produced
// by `lit snapshots new`). It is the precise complement of
// store.IsMigrationSnapshotName, kept as a separate predicate so the
// kind-aware Prune call site reads as "prune user snapshots" instead of
// "prune the not-migration ones".
//
// [LAW:one-source-of-truth] Both producers' kind classifiers route through
// store.IsMigrationSnapshotName; this helper is just the complement,
// expressed for callsite clarity.
func isUserSnapshotName(name string) bool {
	return !store.IsMigrationSnapshotName(name)
}

func runSnapshots(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit snapshots <new|list|restore> ...")
	}
	switch args[0] {
	case "new":
		return runSnapshotsNew(ctx, stdout, ws, args[1:])
	case "list":
		return runSnapshotsList(stdout, ws, args[1:])
	case "restore":
		return runSnapshotsRestore(ctx, stdout, ws, args[1:])
	default:
		return errors.New("usage: lit snapshots <new|list|restore> ...")
	}
}

// withCommitLock acquires the path-based commit lock used by Store mutations
// so a clone/restore can't interleave with concurrent writes from `lit update`
// or any other in-process mutation. Routes through store.LockCommitPath so the
// lock primitive stays single-source.
//
// KNOWN LIMITATION: the commit lock serializes against writers but not against
// concurrent readers (`lit ls` / `lit show` open a Dolt SQL connection that
// outlives any held lock). A concurrent reader during `lit snapshots restore`
// can observe a renamed-out-from-under-it database directory; the failure mode
// is a query error, not silent corruption. A workspace-exclusivity lock that
// every Store holds for its lifetime is tracked under
// links-schema-rebuild-r5v9.7.
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
	cfg, err := config.Load(ws.RootDir)
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

func runSnapshotsRestore(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
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
