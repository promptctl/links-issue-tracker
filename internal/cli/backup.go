package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/backup"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/statusline"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/syncfile"
)

var backupFamily = commandFamily[appSubcommand]{
	usage: "usage: lit backup <create|list|restore> ...",
	subcommands: []subcommandRow[appSubcommand]{
		// create only reads the store: it exports issue data and writes the
		// snapshot file outside the database, so a write lock is unnecessary.
		{name: "create", payload: appSubcommand{access: app.AccessRead, declare: backupCreateLeaf}},
		{name: "list", payload: appSubcommand{access: app.AccessRead, declare: backupListLeaf}},
		{name: "restore", payload: appSubcommand{access: app.AccessWrite, declare: backupRestoreLeaf}},
	},
}

func backupCreateLeaf() appLeaf {
	fs := newCobraFlagSet("backup create")
	keep := fs.Int("keep", 20, "Snapshots to keep after rotation")
	return appLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		export, err := ap.Store.Export(ctx)
		if err != nil {
			return err
		}
		snapshot, err := backup.Create(ap.Workspace.StorageDir, export)
		if err != nil {
			return err
		}
		if err := backup.Prune(ap.Workspace.StorageDir, *keep); err != nil {
			return err
		}
		_, err = fmt.Fprintf(inv.Stdout, "%s %s\n", snapshot.Name, snapshot.Path)
		return err
	}}
}

func backupListLeaf() appLeaf {
	fs := newCobraFlagSet("backup list")
	return appLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		snapshots, err := backup.List(ap.Workspace.StorageDir)
		if err != nil {
			return err
		}
		for _, snapshot := range snapshots {
			if _, err := fmt.Fprintf(inv.Stdout, "%s %d %s\n", snapshot.Name, snapshot.Size, snapshot.Path); err != nil {
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

// resolveRestorePath is the single authority that turns a restore source into a
// path. The two sources are the only ones there can be: restoreFromExportPath
// reads any model.Export JSON identically and is blind to whether the file came
// from `backup create` or the sync engine, so a file's provenance is not a
// behavioral axis and earns no separate flag. [LAW:no-silent-failure] passing
// both sources is an explicit error, never a silent precedence between them.
func resolveRestorePath(ap *app.App, explicitPath string, latest bool) (string, error) {
	path := strings.TrimSpace(explicitPath)
	if latest {
		if path != "" {
			return "", UsageError{Message: restoreUsage + " — --latest and --path are mutually exclusive"}
		}
		snapshot, err := backup.Latest(ap.Workspace.StorageDir)
		if err != nil {
			return "", err
		}
		if snapshot == nil {
			return "", errors.New("no backups available")
		}
		return snapshot.Path, nil
	}
	if path == "" {
		return "", UsageError{Message: restoreUsage}
	}
	return path, nil
}

func backupRestoreLeaf() appLeaf {
	fs := newCobraFlagSet("backup restore")
	path := registerPath(fs, "path", "Path to an export JSON (backup snapshot or sync file)")
	latest := fs.Bool("latest", false, "Restore latest backup snapshot")
	force := fs.Bool("force", false, "Restore over changes made since the last restore")
	return appLeaf{fs: fs, positionals: 0, usage: restoreUsage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		restorePath, err := resolveRestorePath(ap, path.path(), *latest)
		if err != nil {
			return err
		}
		restored, err := restoreFromExportPath(ctx, inv.Env, ap, restorePath, *force)
		if err == nil {
			_, err = fmt.Fprintf(inv.Stdout, "restored %s\n", restorePath)
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

// restoreFromExportPath restores the store from the export at path and returns
// the restore's outcome, failed or not. Every restore leaves one sync trace
// record, failed or not: it records the digest the next restore's guard
// compares against, and the record carries the verdict and the changes.
// [LAW:nothing-unseen]
func restoreFromExportPath(ctx context.Context, env environ.Env, ap *app.App, path string, force bool) (outcome restoreOutcome, err error) {
	defer func() {
		metadata := map[string]string{"path": filepath.Clean(path)}
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
		recordSyncCommandTrace(env, ap.Workspace, "lit backup restore", "restored", err, metadata)
	}()
	return restoreFromExport(ctx, ap, path, force)
}

func restoreFromExport(ctx context.Context, ap *app.App, path string, force bool) (restoreOutcome, error) {
	restorePath := filepath.Clean(path)
	var outcome restoreOutcome
	// [LAW:parse-dont-validate] A restore needs the import capability: the
	// replace, and the digest of the last restore it compares the workspace
	// against. It is asked for before anything is read, written, or backed up,
	// and what comes back is the interface, so nothing below re-asks.
	importer, err := storage.Import.Of(ap.Store)
	if err != nil {
		return outcome, err
	}
	targetExport, _, err := syncfile.Read(restorePath)
	if err != nil {
		return outcome, err
	}
	localExport, err := ap.Store.Export(ctx)
	if err != nil {
		return outcome, err
	}
	recorded, err := importer.RestoreDigest(ctx)
	if err != nil {
		return outcome, err
	}
	outcome.guard, err = guardRestore(recorded, localExport, force)
	if err != nil {
		return outcome, err
	}
	if outcome.guard == guardChanged {
		return outcome, RestoreOverwriteError{}
	}
	snapshot, err := backup.Create(ap.Workspace.StorageDir, localExport)
	if err != nil {
		return outcome, err
	}
	outcome.snapshot = snapshot.Path
	if err := backup.Prune(ap.Workspace.StorageDir, 20); err != nil {
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
	return outcome, importer.RecordRestoreDigest(ctx, digest)
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

// guardRestore compares what the workspace holds with recorded, the content
// digest the last restore took of what it left. local must come from the
// store: syncfile.ContentHash cannot hash a decoded file.
func guardRestore(recorded string, local model.Export, force bool) (restoreGuard, error) {
	switch {
	case force:
		return guardForced, nil
	case recorded == "":
		return guardUnrecorded, nil
	}
	hash, err := syncfile.ContentHash(local)
	if err != nil {
		return "", err
	}
	if hash != recorded {
		return guardChanged, nil
	}
	return guardUnchanged, nil
}
