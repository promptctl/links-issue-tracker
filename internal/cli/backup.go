package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	force := fs.Bool("force", false, "Force restore over unsynced state")
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
		// and moved what it did, so the outcome is reported whether or not the
		// restore finished; one that committed nothing reports nothing.
		// [LAW:no-silent-failure]
		if reportErr := writeDropped(inv.Stdout, restored.dropped, restored.snapshot); reportErr != nil {
			return errors.Join(err, reportErr)
		}
		if reportErr := writeLanding(inv.Stdout, "the backup", restored.landing); reportErr != nil {
			return errors.Join(err, reportErr)
		}
		return err
	}}
}

func syncBasePath(ap *app.App) string {
	return filepath.Join(ap.Workspace.StorageDir, "last-sync-base.json")
}

// restoreOutcome is what a restore did beyond loading the file: the tickets it
// destroyed, what it changed to land the file, and the pre-restore backup that
// still holds the workspace it replaced. dropped and landing are set only once
// the replace commits, so a refused or failed-before-replace restore carries
// neither.
type restoreOutcome struct {
	dropped  []model.Issue
	landing  storage.Landing
	snapshot string
}

// restoreFromExportPath restores the store from the export at path and returns
// the restore's outcome, failed or not. Every restore leaves one sync trace
// record, failed or not: it rewrites the sync base the next push compares
// against, and the record carries the changes. [LAW:nothing-unseen]
func restoreFromExportPath(ctx context.Context, env environ.Env, ap *app.App, path string, force bool) (outcome restoreOutcome, err error) {
	metadata := map[string]string{"path": filepath.Clean(path)}
	defer func() {
		landingMetadata(metadata, outcome.landing)
		recordSyncCommandTrace(env, ap.Workspace, "lit backup restore", "restored", err, metadata)
	}()
	return restoreFromExport(ctx, ap, path, force, metadata)
}

// restoreFromExport records on metadata each fact the restore learns, as it
// learns it, so a restore that fails before a fact exists leaves its key out
// rather than recording a zero nobody counted. [LAW:nothing-unseen]
func restoreFromExport(ctx context.Context, ap *app.App, path string, force bool, metadata map[string]string) (restoreOutcome, error) {
	restorePath := filepath.Clean(path)
	var outcome restoreOutcome
	// [LAW:parse-dont-validate] A restore needs two capabilities: the sync state
	// it compares the local workspace against, and the import that replaces the
	// contents. Both are asked for before anything is read, written, or backed
	// up, so an engine offering one and not the other cannot strand a restore
	// half-done. What comes back is the interface, so nothing below re-asks.
	syncer, err := storage.Sync.Of(ap.Store)
	if err != nil {
		return outcome, err
	}
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
	absent := droppedBy(localExport, targetExport)
	absentMetadata(metadata, absent)
	state, err := syncer.GetSyncState(ctx)
	if err != nil {
		return outcome, err
	}
	if state.ContentHash != "" && !force {
		baseHash, hashErr := syncfile.HashFile(syncBasePath(ap))
		if hashErr != nil {
			return outcome, hashErr
		}
		if baseHash != "" {
			localHash, localHashErr := hashExport(localExport)
			if localHashErr != nil {
				return outcome, localHashErr
			}
			if localHash != baseHash {
				return outcome, MergeConflictError{Message: "restore conflict: local workspace has unsynced changes since last sync base"}
			}
		}
	}
	snapshot, err := backup.Create(ap.Workspace.StorageDir, localExport)
	if err != nil {
		return outcome, err
	}
	outcome.snapshot = snapshot.Path
	if err := backup.Prune(ap.Workspace.StorageDir, 20); err != nil {
		return outcome, err
	}
	outcome.landing, err = importer.ReplaceFromExport(ctx, targetExport)
	if err != nil {
		return outcome, err
	}
	outcome.dropped = absent
	ap.Statusline.Record(statusline.Replaced{})
	// The landing is committed from here on, so every later failure still
	// returns it: the trace records what the store now holds, not what the
	// command finished.
	// [LAW:single-enforcer] Restored sync base is serialized from the store so container lifecycles pass through the hydration boundary before JSON output.
	restoredExport, err := ap.Store.Export(ctx)
	if err != nil {
		return outcome, err
	}
	if _, err := syncfile.WriteAtomic(syncBasePath(ap), restoredExport); err != nil {
		return outcome, err
	}
	hash, err := syncfile.HashFile(restorePath)
	if err != nil {
		return outcome, err
	}
	return outcome, syncer.RecordSyncState(ctx, storage.SyncState{
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
