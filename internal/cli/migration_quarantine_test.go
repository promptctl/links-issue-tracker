package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/version"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
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
		if ev.ExitCode != ExitValidation || ev.Reason != reasonMigrationQuarantined.String() {
			t.Errorf("Run(%v) event = exit %d reason %q, want exit %d reason %q", args, ev.ExitCode, ev.Reason, ExitValidation, reasonMigrationQuarantined)
		}
	}
}

// quarantineTop reverses the workspace's top n migrations and quarantines
// each, the state failed opens leave behind, and returns the lowest of them.
func quarantineTop(t *testing.T, ws workspace.Info, n int64) int64 {
	t.Helper()
	info, err := version.Get()
	if err != nil {
		t.Fatalf("version.Get() error = %v", err)
	}
	first := info.Schema.Max - n + 1
	ctx := context.Background()
	st, err := engine.Open(ctx, engine.ReadWrite, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	migrator, err := storage.SchemaMigration.Of(st)
	if err != nil {
		t.Fatalf("schema migration: %v", err)
	}
	if _, err := migrator.Downgrade(ctx, first-1); err != nil {
		t.Fatalf("Downgrade(v%d) error = %v", first-1, err)
	}
	raw, err := storage.TestSupport.Of(st)
	if err != nil {
		t.Fatalf("test support: %v", err)
	}
	for v := first; v <= info.Schema.Max; v++ {
		if err := raw.ExecRawForTest(ctx, "INSERT INTO migration_quarantine (version, name, error_text, created_at) VALUES (?, ?, 'simulated failure', '2026-01-01T00:00:00Z')", v, fmt.Sprintf("v%d.sql", v)); err != nil {
			t.Fatalf("seed quarantine of v%d: %v", v, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}
	return first
}

// `lit unquarantine` on a workspace whose pending migration is quarantined
// clears that quarantine, says the migration runs again, records the version
// on its event, and the next command opens the store and runs it.
func TestUnquarantineClearsTheQuarantineSoTheNextCommandRunsTheMigration(t *testing.T) {
	isolateEvents(t)
	repo, ws := initBootstrapTestRepo(t)
	v := quarantineTop(t, ws, 1)

	out := runCLIInDir(t, repo, "unquarantine")
	want := fmt.Sprintf("cleared the quarantine of migration v%d \"v%d.sql\"\nthe next command that opens the store runs it again\n", v, v)
	if out != want {
		t.Errorf("lit unquarantine output = %q, want %q", out, want)
	}
	ev := lastEvent(t)
	if got := ev.Facts["unquarantine.cleared_version"]; got != float64(v) {
		t.Errorf("event unquarantine.cleared_version = %v, want %d", got, v)
	}
	if got, present := ev.Facts["unquarantine.next_version"]; present {
		t.Errorf("event carries unquarantine.next_version = %v with no quarantine left", got)
	}
	runCLIInDir(t, repo, "ls")
}

// With a second quarantine in force, `lit unquarantine` clears the first and
// says the next command is refused on the second, never that it runs again.
func TestUnquarantineNamesTheQuarantineStillInForce(t *testing.T) {
	isolateEvents(t)
	repo, ws := initBootstrapTestRepo(t)
	v := quarantineTop(t, ws, 2)

	out := runCLIInDir(t, repo, "unquarantine")
	want := fmt.Sprintf("cleared the quarantine of migration v%d \"v%d.sql\"\nmigration v%d \"v%d.sql\" is still quarantined, so the next command that opens the store refuses on it; `lit unquarantine` clears it\n", v, v, v+1, v+1)
	if out != want {
		t.Errorf("lit unquarantine output = %q, want %q", out, want)
	}
	if got := lastEvent(t).Facts["unquarantine.next_version"]; got != float64(v+1) {
		t.Errorf("event unquarantine.next_version = %v, want %d", got, v+1)
	}
}

// `lit unquarantine` with nothing quarantined answers that the state already
// holds: exit 6, never a retry.
func TestUnquarantineWithNothingQuarantinedExitsStateAlreadyHolds(t *testing.T) {
	isolateEvents(t)
	repo, _ := initBootstrapTestRepo(t)
	_, err := runCLIInDirAllowError(t, repo, "unquarantine")
	var none store.NoQuarantineError
	if !errors.As(err, &none) {
		t.Fatalf("lit unquarantine error = %v (%T), want store.NoQuarantineError", err, err)
	}
	if ev := lastEvent(t); ev.ExitCode != ExitNoWork || ev.Reason != reasonStateAlreadyHolds.String() {
		t.Errorf("event = exit %d reason %q, want exit %d reason %q", ev.ExitCode, ev.Reason, ExitNoWork, reasonStateAlreadyHolds)
	}
}
