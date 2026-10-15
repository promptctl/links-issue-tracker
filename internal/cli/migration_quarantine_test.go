package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// A workspace whose pending migration is quarantined refuses at exit 3 with
// its own reason, and the invocation's event carries both. The workspace is
// built for real and the command runs through the CLI, so this also pins that
// the store's refusal reaches the CLI with its type intact.
func TestAQuarantinedMigrationRefusesAtExit3WithItsOwnReason(t *testing.T) {
	isolateEvents(t)
	repo, ws := initBootstrapTestRepo(t)
	info, err := version.Get()
	if err != nil {
		t.Fatalf("version.Get() error = %v", err)
	}
	latest := info.Schema.Max
	ctx := context.Background()
	st, err := engine.Open(ctx, engine.ReadWrite, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	raw, err := storage.TestSupport.Of(st)
	if err != nil {
		t.Fatalf("test support: %v", err)
	}
	// The latest migration pending again and quarantined: the state a startup
	// migration that failed leaves behind.
	for _, stmt := range []string{
		"DELETE FROM goose_db_version WHERE version_id = ?",
		"INSERT INTO migration_quarantine (version, name, error_text, created_at) VALUES (?, 'latest.sql', 'simulated failure', '2026-01-01T00:00:00Z')",
	} {
		if err := raw.ExecRawForTest(ctx, stmt, latest); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}

	for _, args := range [][]string{{"ls"}, {"new", "--type", "epic", "--title", "e", "--topic", "demo"}} {
		_, err := runCLIInDirAllowError(t, repo, args...)
		var quarantined *store.QuarantineBlockError
		if !errors.As(err, &quarantined) || quarantined.Version != latest {
			t.Fatalf("Run(%v) error = %v (%T), want the quarantine of v%d", args, err, err, latest)
		}
		ev := lastEvent(t)
		if ev.ExitCode != ExitValidation || ev.Reason != string(reasonMigrationQuarantined) {
			t.Errorf("Run(%v) event = exit %d reason %q, want exit %d reason %q", args, ev.ExitCode, ev.Reason, ExitValidation, reasonMigrationQuarantined)
		}
	}
}
