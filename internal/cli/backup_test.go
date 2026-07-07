package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/backup"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// resolveRestorePath is the single authority `backup restore` resolves its
// source through; these assert the documented cap of two sources (explicit
// path, latest backup) and that the degenerate combinations fail loudly rather
// than picking a silent precedence.
func TestResolveRestorePathCap(t *testing.T) {
	t.Run("no source is a usage error", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{StorageDir: t.TempDir()}}
		_, err := resolveRestorePath(ap, "  ", false)
		if _, ok := err.(UsageError); !ok {
			t.Fatalf("resolveRestorePath(no source) error = %v, want UsageError", err)
		}
	})

	t.Run("both sources is a mutual-exclusion error, not silent precedence", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{StorageDir: t.TempDir()}}
		_, err := resolveRestorePath(ap, "/some/export.json", true)
		ue, ok := err.(UsageError)
		if !ok || !strings.Contains(ue.Message, "mutually exclusive") {
			t.Fatalf("resolveRestorePath(both) error = %v, want mutually-exclusive UsageError", err)
		}
	})

	t.Run("explicit path passes through trimmed", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{StorageDir: t.TempDir()}}
		got, err := resolveRestorePath(ap, "  /some/export.json  ", false)
		if err != nil || got != "/some/export.json" {
			t.Fatalf("resolveRestorePath(path) = %q, %v; want trimmed path", got, err)
		}
	})

	t.Run("latest with no backups fails loudly", func(t *testing.T) {
		ap := &app.App{Workspace: workspace.Info{StorageDir: t.TempDir()}}
		_, err := resolveRestorePath(ap, "", true)
		if err == nil || !strings.Contains(err.Error(), "no backups available") {
			t.Fatalf("resolveRestorePath(latest, empty) error = %v, want no backups available", err)
		}
	})

	t.Run("latest resolves to the newest snapshot path", func(t *testing.T) {
		dir := t.TempDir()
		ap := &app.App{Workspace: workspace.Info{StorageDir: dir}}
		export := model.Export{Version: 1, WorkspaceID: "ws", ExportedAt: time.Now().UTC()}
		snapshot, err := backup.Create(dir, export)
		if err != nil {
			t.Fatalf("backup.Create() error = %v", err)
		}
		got, err := resolveRestorePath(ap, "", true)
		if err != nil || got != snapshot.Path {
			t.Fatalf("resolveRestorePath(latest) = %q, %v; want %q", got, err, snapshot.Path)
		}
	})
}

func TestHashExportRefusesUnhydratedIssue(t *testing.T) {
	// hashExport relies on Issue.MarshalJSON to reject unhydrated values; the
	// hydrator's post-condition keeps this path unreachable from store output,
	// but the JSON boundary still enforces if any other producer slips one in.
	export := model.Export{
		Version:     1,
		WorkspaceID: "ws",
		ExportedAt:  time.Now().UTC(),
		Issues:      []model.Issue{{ID: "unhydrated-x", IssueType: "task"}},
	}
	_, err := hashExport(export)
	if err == nil || !strings.Contains(err.Error(), "no hydrated lifecycle") {
		t.Fatalf("hashExport error = %v, want no hydrated lifecycle error", err)
	}
}
