package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/backup"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/syncfile"
)

var backupFamily = commandFamily[appSubcommand]{
	usage: "usage: lit backup <create|list|restore> ...",
	subcommands: []subcommandRow[appSubcommand]{
		// create only reads the store: it exports issue data and writes the
		// snapshot file outside the database, so a write lock is unnecessary.
		{name: "create", payload: appSubcommand{access: app.AccessRead, run: runBackupCreate}},
		{name: "list", payload: appSubcommand{access: app.AccessRead, run: runBackupList}},
		{name: "restore", payload: appSubcommand{access: app.AccessWrite, run: runBackupRestore}},
	},
}

func runBackupCreate(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("backup create")
	keep := fs.Int("keep", 20, "Snapshots to keep after rotation")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
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
	_, err = fmt.Fprintf(stdout, "%s %s\n", snapshot.Name, snapshot.Path)
	return err
}

func runBackupList(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("backup list")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	snapshots, err := backup.List(ap.Workspace.StorageDir)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		if _, err := fmt.Fprintf(stdout, "%s %d %s\n", snapshot.Name, snapshot.Size, snapshot.Path); err != nil {
			return err
		}
	}
	return nil
}

// restoreSourceUsage is the one canonical restore surface shared by `backup
// restore` and `recover`. [LAW:no-mode-explosion] the cap is two sources — an
// explicit export path or the latest backup snapshot — and it lives here once
// so neither command can grow a private fourth flag without editing this line.
const restoreSourceUsage = "(--latest | --path <snapshot.json>) [--force]"

// resolveRestorePath is the single authority that turns a restore source into a
// path. The two sources are the only ones there can be: restoreFromExportPath
// reads any model.Export JSON identically and is blind to whether the file came
// from `backup create` or the sync engine, so a file's provenance is not a
// behavioral axis and earns no separate flag. [LAW:one-source-of-truth] both
// restore commands resolve here, so the overlap is a declared alias rather than
// two surfaces that drift. [LAW:no-silent-failure] passing both sources is an
// explicit error, never a silent precedence between them.
func resolveRestorePath(ap *app.App, explicitPath string, latest bool, usage string) (string, error) {
	path := strings.TrimSpace(explicitPath)
	if latest {
		if path != "" {
			return "", UsageError{Message: "usage: " + usage + " — --latest and --path are mutually exclusive"}
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
		return "", UsageError{Message: "usage: " + usage}
	}
	return path, nil
}

func runBackupRestore(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("backup restore")
	path := fs.String("path", "", "Backup snapshot path")
	latest := fs.Bool("latest", false, "Restore latest backup snapshot")
	force := fs.Bool("force", false, "Force restore over unsynced state")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	restorePath, err := resolveRestorePath(ap, *path, *latest, "lit backup restore "+restoreSourceUsage)
	if err != nil {
		return err
	}
	if err := restoreFromExportPath(ctx, ap, restorePath, *force); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "restored %s\n", restorePath)
	return err
}

// runRecover is the top-level disaster-recovery alias of `backup restore`: same
// canonical source surface, same resolver, same operation. It exists as its own
// discoverable verb (downgrade failures point users at it), differing only in
// that --path here accepts any export JSON — a backup snapshot or a sync file.
func runRecover(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("recover")
	path := fs.String("path", "", "Export snapshot path (backup snapshot or sync file)")
	latest := fs.Bool("latest", false, "Recover from latest backup snapshot")
	force := fs.Bool("force", false, "Force restore over unsynced state")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	restorePath, err := resolveRestorePath(ap, *path, *latest, "lit recover "+restoreSourceUsage)
	if err != nil {
		return err
	}
	if err := restoreFromExportPath(ctx, ap, restorePath, *force); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "recovered %s\n", restorePath)
	return err
}

func syncBasePath(ap *app.App) string {
	return filepath.Join(ap.Workspace.StorageDir, "last-sync-base.json")
}

func restoreFromExportPath(ctx context.Context, ap *app.App, path string, force bool) error {
	restorePath := filepath.Clean(path)
	targetExport, _, err := syncfile.Read(restorePath)
	if err != nil {
		return err
	}
	localExport, err := ap.Store.Export(ctx)
	if err != nil {
		return err
	}
	state, err := ap.Store.GetSyncState(ctx)
	if err != nil {
		return err
	}
	if state.ContentHash != "" && !force {
		baseHash, hashErr := syncfile.HashFile(syncBasePath(ap))
		if hashErr != nil {
			return hashErr
		}
		if baseHash != "" {
			localHash, localHashErr := hashExport(localExport)
			if localHashErr != nil {
				return localHashErr
			}
			if localHash != baseHash {
				return MergeConflictError{Message: "restore conflict: local workspace has unsynced changes since last sync base"}
			}
		}
	}
	if _, err := backup.Create(ap.Workspace.StorageDir, localExport); err != nil {
		return err
	}
	if err := backup.Prune(ap.Workspace.StorageDir, 20); err != nil {
		return err
	}
	if err := ap.Store.ReplaceFromExport(ctx, targetExport); err != nil {
		return err
	}
	// [LAW:single-enforcer] Restored sync base is serialized from the store so container lifecycles pass through the hydration boundary before JSON output.
	restoredExport, err := ap.Store.Export(ctx)
	if err != nil {
		return err
	}
	if _, err := syncfile.WriteAtomic(syncBasePath(ap), restoredExport); err != nil {
		return err
	}
	hash, err := syncfile.HashFile(restorePath)
	if err != nil {
		return err
	}
	return ap.Store.RecordSyncState(ctx, store.SyncState{
		Path:        restorePath,
		ContentHash: hash,
	})
}

func hashExport(export model.Export) (string, error) {
	// Issue.MarshalJSON refuses partial values, so MarshalIndent below surfaces
	// any unhydrated input as a marshal error — no need for a duplicate guard here.
	payload, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal export: %w", err)
	}
	payload = append(payload, '\n')
	sum := sha256.Sum256(payload)
	return strings.ToLower(hex.EncodeToString(sum[:])), nil
}
