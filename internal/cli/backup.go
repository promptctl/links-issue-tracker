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

	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/backup"
	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/syncfile"
)

func validateBackupCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit backup <create|list|restore> ...", "create", "list", "restore")
}

func runBackup(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit backup <create|list|restore> ...")
	}
	switch args[0] {
	case "create":
		fs := newCobraFlagSet("backup create")
		keep := fs.Int("keep", 20, "Snapshots to keep after rotation")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
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
		return printValue(stdout, snapshot, *jsonOut, func(w io.Writer, v any) error {
			s := v.(backup.Snapshot)
			_, err := fmt.Fprintf(w, "%s %s\n", s.Name, s.Path)
			return err
		})
	case "list":
		fs := newCobraFlagSet("backup list")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		snapshots, err := backup.List(ap.Workspace.StorageDir)
		if err != nil {
			return err
		}
		return printValue(stdout, snapshots, *jsonOut, func(w io.Writer, v any) error {
			list := v.([]backup.Snapshot)
			for _, snapshot := range list {
				if _, err := fmt.Fprintf(w, "%s %d %s\n", snapshot.Name, snapshot.Size, snapshot.Path); err != nil {
					return err
				}
			}
			return nil
		})
	case "restore":
		fs := newCobraFlagSet("backup restore")
		path := fs.String("path", "", "Backup snapshot path")
		latest := fs.Bool("latest", false, "Restore latest backup snapshot")
		force := fs.Bool("force", false, "Force restore over unsynced state")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		restorePath := strings.TrimSpace(*path)
		if *latest {
			latestSnapshot, err := backup.Latest(ap.Workspace.StorageDir)
			if err != nil {
				return err
			}
			if latestSnapshot == nil {
				return errors.New("no backups available")
			}
			restorePath = latestSnapshot.Path
		}
		if restorePath == "" {
			return errors.New("usage: lit backup restore --path <snapshot.json> [--force] [--json] or --latest")
		}
		if err := restoreFromExportPath(ctx, ap, restorePath, *force); err != nil {
			return err
		}
		payload := map[string]string{"status": "restored", "path": restorePath}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]string)
			_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
			return err
		})
	default:
		return errors.New("usage: lit backup <create|list|restore> ...")
	}
}

func runRecover(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("recover")
	fromSync := fs.String("from-sync", "", "Restore from sync file")
	fromBackup := fs.String("from-backup", "", "Restore from backup snapshot")
	latestBackup := fs.Bool("latest-backup", false, "Restore from latest backup snapshot")
	force := fs.Bool("force", false, "Force restore over unsynced state")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	var restorePath string
	switch {
	case strings.TrimSpace(*fromSync) != "":
		restorePath = strings.TrimSpace(*fromSync)
	case strings.TrimSpace(*fromBackup) != "":
		restorePath = strings.TrimSpace(*fromBackup)
	case *latestBackup:
		latest, err := backup.Latest(ap.Workspace.StorageDir)
		if err != nil {
			return err
		}
		if latest == nil {
			return errors.New("no backups available")
		}
		restorePath = latest.Path
	default:
		return errors.New("usage: lit recover --from-sync <path> | --from-backup <path> | --latest-backup [--force] [--json]")
	}
	if err := restoreFromExportPath(ctx, ap, restorePath, *force); err != nil {
		return err
	}
	payload := map[string]string{"status": "recovered", "path": restorePath}
	return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
		p := v.(map[string]string)
		_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
		return err
	})
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
	for _, issue := range export.Issues {
		if !issue.IsHydrated() {
			return "", fmt.Errorf("hash export: issue %s is not hydrated", issue.ID)
		}
	}
	payload, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal export: %w", err)
	}
	payload = append(payload, '\n')
	sum := sha256.Sum256(payload)
	return strings.ToLower(hex.EncodeToString(sum[:])), nil
}
