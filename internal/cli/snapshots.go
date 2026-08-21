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
	return !store.IsMigrationSnapshotName(name) &&
		!store.IsDowngradeSnapshotName(name) &&
		!store.IsReconcileSnapshotName(name)
}

// withCommitLock acquires the path-based commit lock used by Store mutations
// so a clone/restore can't interleave with concurrent writes from `lit update`
// or any other in-process mutation. Routes through store.LockCommitPath so the
// lock primitive stays single-source.
//
// Reader-vs-rotator exclusion is owned by the workspace lock (shared holds
// for directory readers, exclusive for rotators — the contract and its
// callers live at store/workspace_lock.go); this commit lock remains the
// writer-vs-writer gate only.
func withCommitLock(ctx context.Context, ws workspace.Info, fn func() error) (err error) {
	release, err := store.LockCommitPath(ctx, store.CommitLockPath(ws.DatabasePath))
	if err != nil {
		return err
	}
	// [LAW:single-enforcer] store.SettleCommitLockRelease owns how a release
	// failure combines with fn's outcome — joined beside a failure, demoted to
	// loud stderr after a durable success — so both withCommitLock spellings
	// settle by one rule.
	defer func() {
		err = store.SettleCommitLockRelease(err, release())
	}()
	return fn()
}

func runSnapshotsNew(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("snapshots new")
	label := fs.String("label", "", "Optional human-readable label appended to the snapshot name")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	// [LAW:no-silent-failure] A stray positional is a misfired intent (the
	// sibling restore takes its argument positionally, so `snapshots new
	// nightly` is a natural typo for `--label nightly`); accepting it would
	// mint an unlabeled snapshot the operator then can't find by the name
	// they thought they gave it.
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit snapshots new [--label <text>]"}
	}
	cfg, err := config.Load(pathspec.New(ws.RootDir))
	if err != nil {
		return err
	}
	snap, err := takeUserSnapshot(ctx, ws, strings.TrimSpace(*label))
	// The record prints the moment it exists — before the prune, and even
	// beside a failure that landed after the take (a lock release, the
	// retention prune). [FRAMING:representation] the snapshot is durable and
	// listed from the instant Take returned; a non-zero exit that hides its
	// name sends the operator retrying into a duplicate of a snapshot they
	// already have.
	if snap.Name != "" {
		if _, printErr := fmt.Fprintf(stdout, "%s %s\n", snap.Name, snap.Path); printErr != nil {
			err = errors.Join(err, printErr)
		}
	}
	if err != nil {
		return err
	}
	// Prune runs after the workspace hold is released: it deletes only aged
	// snapshot directories under the storage dir and never reads the Dolt
	// directory, so a multi-gigabyte RemoveAll must not keep rotators
	// refusing workspace-busy (their exclusive acquisition is one-attempt).
	// The commit lock still serializes it against concurrent snapshot
	// producers, exactly as before the copy grew its workspace hold.
	//
	// [LAW:single-enforcer] User-snapshot retention bounds *user* snapshots
	// only; migration snapshots share the directory but are pruned
	// independently by migrate() under its own budget. Without the kind
	// filter, `lit snapshots new` could evict a recovery snapshot the
	// migration system is depending on.
	return withCommitLock(ctx, ws, func() error {
		return dbsnapshot.PruneMatching(snapshotsDirFor(ws), cfg.Snapshot.RetentionBudget, isUserSnapshotName)
	})
}

// takeUserSnapshot brackets the Dolt-directory copy in exactly the holds the
// copy needs and releases them at return, so the caller's housekeeping never
// extends the exclusion window past the last directory read.
//
// [LAW:single-enforcer] The copy reads the Dolt directory file by file — the
// same kind of actor as an open Store — so it takes the same shared workspace
// hold every Store open takes, acquired before the commit lock in the same
// workspace→commit order as runSnapshotsRestore and store.Open. That contends
// with every rotator's exclusive hold without blocking ordinary readers; the
// commit lock alone never did, because rotators don't hold it during their
// destructive window (links-sync-pgct.14's torn-snapshot race). The two holds
// own rotator exclusion and writer serialization respectively; engine-lifecycle
// I/O that holds neither (a concurrent open's journal crash-recovery after an
// unclean kill) is a distinct, narrower exposure tracked by links-sync-pgct.15.
func takeUserSnapshot(ctx context.Context, ws workspace.Info, label string) (snap dbsnapshot.Snapshot, err error) {
	releaseWorkspace, err := store.LockWorkspaceShared(ctx, ws.DatabasePath)
	if err != nil {
		return dbsnapshot.Snapshot{}, err
	}
	// [LAW:no-silent-failure] Same release contract as runSnapshotsRestore: a
	// failed release can leave the workspace stuck busy for later commands,
	// so it surfaces via the named return alongside any snapshot error.
	defer func() {
		if relErr := releaseWorkspace(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	// Same post-lock ordering as every store open: only a marker checked
	// under the held workspace lock is binding (a live adopt holds the lock
	// exclusively, so reaching this line proves any marker present belongs
	// to a dead adopt). Without this check the copy would snapshot condemned
	// residue as a "restorable" recovery point — and the retention prune
	// would then evict a good snapshot to keep the garbage one.
	if err := store.PendingAdopt(ws.DatabasePath); err != nil {
		return dbsnapshot.Snapshot{}, err
	}
	err = withCommitLock(ctx, ws, func() error {
		s, err := dbsnapshot.Take(ctx, ws.DatabasePath, snapshotsDirFor(ws), label)
		if err != nil {
			return err
		}
		snap = s
		return nil
	})
	// snap is populated exactly when Take succeeded, so a failure that landed
	// after the take (the commit-lock release here, the workspace release in
	// the defer above) travels beside the record of the durable snapshot it
	// did not undo, never in place of it. [LAW:no-silent-failure]
	return snap, err
}

func runSnapshotsList(stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("snapshots list")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	snapshots, err := dbsnapshot.List(snapshotsDirFor(ws))
	if err != nil {
		return err
	}
	for _, snap := range snapshots {
		if _, err := fmt.Fprintf(stdout, "%s %s %s\n", snap.Name, snap.Created.Format("2006-01-02T15:04:05Z"), snap.Path); err != nil {
			return err
		}
	}
	return nil
}

func runSnapshotsRestore(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) (err error) {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("snapshots restore")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 || fs.NArg() != 0 {
		return UsageError{Message: "usage: lit snapshots restore <name>"}
	}
	name := strings.TrimSpace(positional[0])
	if name == "" {
		return UsageError{Message: "usage: lit snapshots restore <name>"}
	}
	// [LAW:single-enforcer] Exclusive workspace lock owns reader-vs-restore
	// exclusion; commit lock (held inside withCommitLock below) owns
	// writer-vs-restore exclusion. Both held while the Dolt directory is
	// rotated so no Store — open or about to open — can observe the rename.
	releaseWorkspace, err := store.LockWorkspaceExclusive(ctx, ws.DatabasePath)
	if err != nil {
		return err
	}
	// [LAW:no-silent-failure] A release failure is rare but real (e.g.
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
	restored := false
	err = withCommitLock(ctx, ws, func() error {
		// rotated is captured even when Restore then fails: a non-empty path
		// beside an error means the pre-restore directory was already moved
		// aside (Restore's install step failed after its rotation step), and
		// that path is the operator's only pointer to their displaced data.
		r, restoreErr := dbsnapshot.Restore(ws.DatabasePath, snapshotsDirFor(ws), name)
		rotated = r
		restored = restoreErr == nil
		return restoreErr
	})
	if !restored && rotated != "" {
		err = fmt.Errorf("the pre-restore database directory was moved aside to %s before this failure and holds the workspace's data: %w", rotated, err)
	}
	// The record prints the moment the restore is durable, even beside a
	// failure that lands after it (the workspace release joined in the defer
	// above). [FRAMING:representation] the snapshot was consumed and the old
	// directory moved the instant Restore returned; an error exit that hides
	// the record sends the operator retrying a name that now reports missing,
	// with no pointer to the rotated-aside pre-restore directory.
	if restored {
		line := fmt.Sprintf("restored %s\n", name)
		if rotated != "" {
			line = fmt.Sprintf("restored %s rotated_to=%s\n", name, rotated)
		}
		if _, printErr := fmt.Fprint(stdout, line); printErr != nil {
			err = errors.Join(err, printErr)
		}
	}
	return err
}
