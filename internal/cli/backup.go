package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/backup"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/statusline"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/syncfile"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

var backupFamily = commandFamily[appSubcommand]{
	usage: "usage: lit backup <create|list|restore> ...",
	subcommands: []subcommandRow[appSubcommand]{
		// create only reads the store: it exports issue data and writes the
		// snapshot file outside the database, so a write lock is unnecessary.
		// The backups set has its own lock, held by backup.Take.
		{name: "create", payload: appSubcommand{access: app.AccessRead, declare: backupCreateLeaf}},
		{name: "list", payload: appSubcommand{access: app.AccessRead, declare: backupListLeaf}},
		{name: "restore", payload: appSubcommand{access: app.AccessWrite, declare: backupRestoreLeaf}},
	},
}

func backupCreateLeaf() appLeaf {
	fs := newCobraFlagSet("backup create")
	keep := fs.Int("keep", backup.Budget, "Snapshots to keep after rotation")
	return appLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		taken, err := takeBackup(ctx, ap.Workspace, *keep, func() (model.Export, error) { return ap.Store.Export(ctx) })
		// A snapshot that was written is printed even beside a prune that
		// failed after it, as `snapshots new` prints its own.
		if taken != nil {
			_, printErr := fmt.Fprintf(inv.Stdout, "%s %s\n", taken.Name, taken.Path)
			err = errors.Join(err, printErr)
		}
		return err
	}}
}

// takeBackup snapshots what read returns into the workspace's backups, then
// prunes them to keep, through backup.Take, and puts what the take did on the
// invocation's event, for `backup create` and a restore's pre-restore backup
// alike. A nil Written means nothing was written. [LAW:nothing-unseen]
func takeBackup(ctx context.Context, ws workspace.Info, keep int, read func() (model.Export, error)) (*backup.Written, error) {
	taken, err := backup.Take(ctx, ws.StorageDir, keep, read)
	if taken == nil {
		return nil, err
	}
	telemetry.Set(ctx, "backup.lock_wait_ms", taken.Waited.Milliseconds())
	telemetry.Set(ctx, "backup.residue_removed", taken.ResidueRemoved)
	written := taken.Written
	if written == nil {
		return nil, err
	}
	telemetry.Set(ctx, "backup.snapshot", written.Name)
	telemetry.Set(ctx, "backup.refreshed", written.Refreshed)
	telemetry.Set(ctx, "backup.pruned", written.Pruned)
	return written, err
}

func backupListLeaf() appLeaf {
	fs := newCobraFlagSet("backup list")
	return appLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		snapshots, err := backup.List(ap.Workspace.StorageDir)
		if err != nil {
			return err
		}
		for _, snapshot := range snapshots {
			if _, err := fmt.Fprintf(inv.Stdout, "%s %s %d %s\n", snapshot.Name, snapshot.Created.Format(time.RFC3339), snapshot.Size, snapshot.Path); err != nil {
				return err
			}
		}
		return nil
	}}
}

// restoreUsage is the one canonical restore surface. [LAW:no-mode-explosion]
// the cap is two sources — an explicit export path or the latest backup
// snapshot — and a third source is a new alternative in this line, never a
// private flag elsewhere.
const restoreUsage = "usage: lit backup restore (--latest | --path <export.json>) [--force]"

// resolveRestoreSource is the single authority that turns a restore source into
// the export file to restore. The two sources are the only ones there can be:
// restoreFromExportFile decodes any model.Export JSON identically and is blind
// to whether the file came from `backup create` or the sync engine, so a file's
// provenance is not a behavioral axis and earns no separate flag. --path was
// read in the parse; the latest snapshot is read here, because only the opened
// workspace knows which one it is. [LAW:no-silent-failure] passing both sources
// is an explicit error, never a silent precedence between them.
func resolveRestoreSource(ap *app.App, explicit *inputFile, latest bool) (inputFile, error) {
	if latest {
		if explicit != nil {
			return inputFile{}, UsageError{Message: restoreUsage + " — --latest and --path are mutually exclusive"}
		}
		snapshot, err := backup.Latest(ap.Workspace.StorageDir)
		if err != nil {
			return inputFile{}, err
		}
		if snapshot == nil {
			return inputFile{}, errors.New("no backups available")
		}
		data, err := os.ReadFile(snapshot.Path)
		if err != nil {
			return inputFile{}, fmt.Errorf("read latest backup: %w", err)
		}
		return inputFile{path: snapshot.Path, data: data}, nil
	}
	if explicit == nil {
		return inputFile{}, UsageError{Message: restoreUsage}
	}
	return *explicit, nil
}

func backupRestoreLeaf() appLeaf {
	fs := newCobraFlagSet("backup restore")
	path := registerInputFile(fs, "path", "Path to an export JSON (backup snapshot or sync file)")
	latest := fs.Bool("latest", false, "Restore latest backup snapshot")
	force := fs.Bool("force", false, "Restore over changes made since the last restore")
	return appLeaf{fs: fs, positionals: 0, usage: restoreUsage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		source, err := resolveRestoreSource(ap, path.input(), *latest)
		if err != nil {
			return err
		}
		restored, err := restoreFromExportFile(ctx, inv.Stderr, inv.Env, ap, source, *force)
		if err == nil {
			_, err = fmt.Fprintf(inv.Stdout, "restored %s\n", source.path)
		}
		// A replace that committed before a later step failed has still removed
		// and moved what it did, so it is reported whether or not the restore
		// finished. [LAW:no-silent-failure]
		if restored.replacement != nil {
			if reportErr := writeReplacement(inv.Stdout, *restored.replacement, restored.snapshot); reportErr != nil {
				return errors.Join(err, reportErr)
			}
		}
		return err
	}}
}

// restoreOutcome is what a restore did beyond loading the file: its guard's
// verdict, what its replace removed and changed, and the pre-restore backup
// taken of the workspace it replaced. replacement is nil until the replace
// commits, so a replace that removed and changed nothing is told apart from
// one that never ran. [LAW:types-are-the-program]
type restoreOutcome struct {
	guard       restoreGuard
	replacement *storage.Replacement
	snapshot    string
}

// restoreFromExportFile restores the store from the export file and returns
// the restore's outcome, failed or not. Every restore leaves one sync trace
// record, failed or not: it records the digest the next restore's guard
// compares against, and the record carries the verdict and the changes.
// [LAW:nothing-unseen]
func restoreFromExportFile(ctx context.Context, stderr io.Writer, env environ.Env, ap *app.App, source inputFile, force bool) (outcome restoreOutcome, err error) {
	defer func() {
		metadata := map[string]string{"path": filepath.Clean(source.path)}
		// The guard's verdict says why a restore went ahead or was refused; a
		// restore that failed before the guard ran has none to record.
		if outcome.guard != "" {
			metadata["restore_guard"] = string(outcome.guard)
		}
		// A restore whose replace never committed counted nothing, so its record
		// carries no counts rather than zeros nobody counted.
		if outcome.replacement != nil {
			replacementMetadata(metadata, *outcome.replacement)
		}
		recordSyncCommandTrace(stderr, env, ap.Workspace, "lit backup restore", "restored", err, metadata)
	}()
	return restoreFromExport(ctx, ap, source.data, force)
}

func restoreFromExport(ctx context.Context, ap *app.App, payload []byte, force bool) (restoreOutcome, error) {
	var outcome restoreOutcome
	// [LAW:parse-dont-validate] A restore needs the import capability. It is
	// asked for before the export is decoded or anything is written or backed
	// up, and what comes back is the interface, so nothing below re-asks.
	importer, err := storage.Import.Of(ap.Store)
	if err != nil {
		return outcome, err
	}
	targetExport, _, err := syncfile.Decode(payload)
	if err != nil {
		return outcome, err
	}
	localExport, err := ap.Store.Export(ctx)
	if err != nil {
		return outcome, err
	}
	outcome.guard, err = guardRestore(ap.Workspace, localExport, force)
	if err != nil {
		return outcome, err
	}
	if outcome.guard == guardChanged {
		return outcome, RestoreOverwriteError{}
	}
	// The pre-restore backup is of the state the guard judged, which is the
	// state the replace below overwrites. Read before the backups lock, it is
	// still the store's state: this command's write Store holds Dolt's LOCK
	// from open to close, so no other writer commits in between (the lock
	// discipline in the store package doc).
	taken, err := takeBackup(ctx, ap.Workspace, backup.Budget, func() (model.Export, error) { return localExport, nil })
	if taken != nil {
		outcome.snapshot = taken.Path
	}
	if err != nil {
		return outcome, err
	}
	replacement, err := importer.ReplaceFromExport(ctx, targetExport)
	if err != nil {
		return outcome, err
	}
	outcome.replacement = &replacement
	ap.Statusline.Record(statusline.Replaced{})
	// The landing is committed from here on, so every later failure still
	// returns it: the trace records what the store now holds, not what the
	// command finished.
	// [LAW:one-source-of-truth] The recorded digest is of what the store now
	// holds, the one record the next restore's guard compares against. It is
	// taken from a fresh store export, never from the file just restored: an
	// epic decoded from a file awaits hydration and cannot be marshalled.
	restoredExport, err := ap.Store.Export(ctx)
	if err != nil {
		return outcome, err
	}
	digest, err := syncfile.ContentHash(restoredExport)
	if err != nil {
		return outcome, err
	}
	return outcome, writeMarkerAtomic(restoreDigestPath(ap.Workspace), []byte(digest+"\n"))
}

// restoreDigestPath is this clone's record of what its last restore left: the
// content digest the next restore's guard compares against. It is a file in
// the storage dir, never a row in the store, because the store syncs and the
// record is of this clone's restore alone: a clone that pulled another's
// digest would be refused over a restore it never ran.
// [LAW:one-source-of-truth]
func restoreDigestPath(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "restore.digest")
}

// restoreGuard is the overwrite guard's verdict on a restore: which check let
// it through, or that the workspace moved since the last restore. Only
// guardChanged refuses. The zero value is a guard that never ran.
// [LAW:types-are-the-program]
type restoreGuard string

const (
	guardForced     restoreGuard = "forced"     // --force skips the comparison
	guardUnrecorded restoreGuard = "unrecorded" // no restore recorded a digest to compare against
	guardUnchanged  restoreGuard = "unchanged"  // the workspace holds what the last restore left
	guardChanged    restoreGuard = "changed"    // the workspace moved since the last restore
)

// guardRestore compares what the workspace holds with the content digest the
// last restore in ws took of what it left. Reading that record is part of the
// comparison --force skips, so an unreadable record never blocks a forced
// restore. local must come from the store: syncfile.ContentHash cannot hash a
// decoded file.
func guardRestore(ws workspace.Info, local model.Export, force bool) (restoreGuard, error) {
	if force {
		return guardForced, nil
	}
	recorded, err := os.ReadFile(restoreDigestPath(ws))
	if errors.Is(err, fs.ErrNotExist) {
		return guardUnrecorded, nil
	}
	if err != nil {
		return "", fmt.Errorf("read the last restore's digest: %w", err)
	}
	hash, err := syncfile.ContentHash(local)
	if err != nil {
		return "", err
	}
	if hash != strings.TrimSpace(string(recorded)) {
		return guardChanged, nil
	}
	return guardUnchanged, nil
}
