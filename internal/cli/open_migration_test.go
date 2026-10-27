package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/fsperm"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/version"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// A command whose open migrates the workspace and then cannot roll an old
// migration snapshot off still runs: it exits 0, its stdout is its own output
// alone, the warning is on stderr, and its event names the snapshot and the
// failure. The command is a read, so this also drives the read open's hand-off
// to the write open that migrates.
func TestACommandWhoseMigratingOpenCannotPruneStillRuns(t *testing.T) {
	isolateEvents(t)
	repo, _, stuck := workspaceWhosePruneWillFail(t)

	out, stderr, err := runCLIInDirSplit(t, repo, "export")
	if err != nil {
		t.Fatalf("lit export error = %v, want it to run\noutput:\n%s", err, out)
	}
	var export map[string]any
	if jerr := json.Unmarshal([]byte(out), &export); jerr != nil {
		t.Errorf("lit export stdout is not the export alone: %v\n%s", jerr, out)
	}
	if !strings.HasPrefix(stderr, "warning: old migration snapshots or checkpoints were not removed: prune migration snapshots: ") {
		t.Errorf("stderr = %q, want it to begin with the prune warning", stderr)
	}
	assertMigrationPruneFailureRecorded(t, stuck)
}

// An open that migrates and then fails on the checkout's identity returns no
// App, so nothing reads the migration's report: the failed prune rides the
// command's error instead of vanishing, since the next open migrates nothing.
func TestAnOpenThatFailsAfterMigratingCarriesThePruneFailure(t *testing.T) {
	isolateEvents(t)
	repo, ws, _ := workspaceWhosePruneWillFail(t)
	if err := os.WriteFile(filepath.Join(ws.PrivateGitDir, "lit-stream"), []byte("not a token"), 0o644); err != nil {
		t.Fatalf("damage stream token: %v", err)
	}

	out, err := runCLIInDirAllowError(t, repo, "ls")
	if err == nil {
		t.Fatalf("lit ls succeeded over a damaged identity\noutput:\n%s", out)
	}
	for _, want := range []string{"malformed", "prune migration snapshots: "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("lit ls error = %q, want it to contain %q", err, want)
		}
	}
}

// lit init's adopt gate counts the local tickets through a read open, which
// migrates a store behind this binary: the gate records that open's migration
// and warns of a failed prune like any other open.
func TestInitsAdoptGateReportsItsOpensMigration(t *testing.T) {
	isolateEvents(t)
	repo, _, stuck := workspaceWhosePruneWillFail(t)

	out, stderr, err := runCLIInDirSplit(t, repo, "init")
	if err != nil {
		t.Fatalf("lit init error = %v, want it to run\noutput:\n%s", err, out)
	}
	if !strings.Contains(stderr, "warning: old migration snapshots or checkpoints were not removed: prune migration snapshots: ") {
		t.Errorf("stderr = %q, want the prune warning", stderr)
	}
	assertMigrationPruneFailureRecorded(t, stuck)
}

// assertMigrationPruneFailureRecorded is the last command's event naming the
// snapshot its open took and the prune that then failed on stuck.
func assertMigrationPruneFailureRecorded(t *testing.T, stuck string) {
	t.Helper()
	ev := lastEvent(t)
	if ev.Outcome != telemetry.OutcomeOK {
		t.Errorf("event outcome = %q, want ok", ev.Outcome)
	}
	if snap, _ := ev.Facts["migration.snapshot"].(string); !strings.Contains(snap, "-pre-migrate-") || snap == filepath.Base(stuck) {
		t.Errorf("migration.snapshot = %v, want the snapshot the open took", ev.Facts["migration.snapshot"])
	}
	if leftOut, ok := ev.Facts["migration.snapshot_left_out"].([]any); !ok || !slices.Contains(leftOut, any("links/.dolt/noms/LOCK")) {
		t.Errorf("migration.snapshot_left_out = %v, want what the snapshot's copy left out, the journal LOCK among it", ev.Facts["migration.snapshot_left_out"])
	}
	if pruneErr, _ := ev.Facts["migration.prune_error"].(string); !strings.HasPrefix(pruneErr, "prune migration snapshots: ") {
		t.Errorf("migration.prune_error = %v, want the prune failure", ev.Facts["migration.prune_error"])
	}
}

// The rollup opens every store it finds, so its event counts the stores whose
// open migrated and then could not prune, and its output notes each.
func TestTheRollupCountsStoresWhoseMigratingOpenCannotPrune(t *testing.T) {
	isolateEvents(t)
	repo, ws, _ := workspaceWhosePruneWillFail(t)

	out, err := runCLIInDirAllowError(t, repo, "stores", "--counts", repo)
	if err != nil {
		t.Fatalf("lit stores --counts error = %v\noutput:\n%s", err, out)
	}
	if want := "~ " + ws.StorageDir + ": warning: old migration snapshots or checkpoints were not removed: prune migration snapshots: "; !strings.Contains(out, want) {
		t.Errorf("lit stores --counts output = %q, want the prune note %q", out, want)
	}
	counts := lastEvent(t).Counts
	for key, want := range map[string]int{"stores.migrated": 1, "stores.prune_failures": 1} {
		if counts[key] != want {
			t.Errorf("counts.%s = %d, want %d", key, counts[key], want)
		}
	}
}

// workspaceWhosePruneWillFail is a workspace one migration behind this binary,
// holding more old migration snapshots than the store keeps, the oldest of
// which cannot be removed: the next open migrates, and its prune fails on that
// snapshot. It returns the repo, its workspace, and that snapshot.
func workspaceWhosePruneWillFail(t *testing.T) (repo string, ws workspace.Info, stuck string) {
	t.Helper()
	repo, ws = initBootstrapTestRepo(t)
	info, err := version.Get()
	if err != nil {
		t.Fatalf("version.Get() error = %v", err)
	}
	ctx := context.Background()
	st, err := engine.Open(ctx, testEnv(), testStderr(t), engine.ReadWrite, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	migrator, err := storage.SchemaMigration.Of(st)
	if err != nil {
		t.Fatalf("schema migration: %v", err)
	}
	if _, err := migrator.Downgrade(ctx, info.Schema.Max-1); err != nil {
		t.Fatalf("Downgrade() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}
	// More than the store's retention budget of ten.
	const planted = 12
	dir := dbsnapshot.DirFor(ws.DatabasePath)
	const base = int64(1700000000000000000)
	stuck = filepath.Join(dir, fmt.Sprintf("%d-pre-migrate-%d", base, base))
	for i := int64(0); i < planted; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%d-pre-migrate-%d", base+i, base+i))
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("plant snapshot: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, "held"), nil, 0o644); err != nil {
			t.Fatalf("plant snapshot file: %v", err)
		}
	}
	fsperm.Deny(t, stuck, fsperm.RemoveAll)
	return repo, ws, stuck
}

// A workspace a newer lit migrated, whose migration changed a write rule this
// lit predates, refuses this lit's writes as a schema-ahead workspace and
// serves its reads, and each command's event says what its open met.
func TestAWorkspaceWhoseWriteRulesThisLitPredatesServesReadsOnly(t *testing.T) {
	isolateEvents(t)
	repo, ws := initBootstrapTestRepo(t)
	current, err := version.Get()
	if err != nil {
		t.Fatalf("version.Get() error = %v", err)
	}
	ahead := current.Schema.Max + 1
	ctx := context.Background()
	st, err := engine.Open(ctx, testEnv(), testStderr(t), engine.ReadWrite, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	raw, err := storage.TestSupport.Of(st)
	if err != nil {
		t.Fatalf("test support: %v", err)
	}
	for _, stmt := range []string{
		fmt.Sprintf("INSERT INTO goose_db_version (version_id, is_applied) VALUES (%d, 1)", ahead),
		fmt.Sprintf("INSERT INTO meta (meta_key, meta_value) VALUES ('min_writer_schema', '%d')", ahead),
	} {
		if err := raw.ExecRawForTest(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}

	out, err := runCLIInDirAllowError(t, repo, "new", "--type", "epic", "--topic", "aud", "--title", "Refused")
	if err == nil {
		t.Fatalf("lit new wrote to a workspace whose write rules it predates\noutput:\n%s", out)
	}
	ev := lastEvent(t)
	if ev.Reason != "workspace_schema_ahead" || ev.ExitCode != ExitValidation || ExitCode(err) != ExitValidation {
		t.Errorf("lit new ended %q, event exit %d, process exit %d; want workspace_schema_ahead exit %d", ev.Reason, ev.ExitCode, ExitCode(err), ExitValidation)
	}
	for key, want := range map[string]any{
		"schema_ahead.workspace_schema":        float64(ahead),
		"schema_ahead.running_schema_max":      float64(current.Schema.Max),
		"schema_ahead.min_writer_schema":       float64(ahead),
		"schema_ahead.min_writer_schema_error": nil,
	} {
		if got, ok := ev.Facts[key]; !ok || got != want {
			t.Errorf("lit new %s = %#v (present %v), want %#v", key, got, ok, want)
		}
	}

	if out, err := runCLIInDirAllowError(t, repo, "ls"); err != nil {
		t.Fatalf("lit ls error = %v, want the read served\noutput:\n%s", err, out)
	}
	ev = lastEvent(t)
	for key, want := range map[string]any{
		"migration.schema_ahead":            float64(ahead),
		"migration.min_writer_schema":       float64(ahead),
		"migration.min_writer_schema_error": nil,
		"migration.read_only":               true,
	} {
		if got, ok := ev.Facts[key]; !ok || got != want {
			t.Errorf("lit ls %s = %#v (present %v), want %#v", key, got, ok, want)
		}
	}
}

// A plain write command, with no sync command run and auto sync off (this
// suite's default), collects the table files a dead writer left staged in the
// store, and its event says how many.
func TestAWriteCommandCollectsTheTempTableFilesADeadWriterLeft(t *testing.T) {
	isolateEvents(t)
	repo, ws := initBootstrapTestRepo(t)
	staged := filepath.Join(store.TempTableFilesDir(ws.DatabasePath), "buffered_file_byte_sink_99")
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(staged, []byte("dead"), 0o600); err != nil {
		t.Fatalf("plant staged file: %v", err)
	}

	runCLIInDir(t, repo, "new", "--title", "after a dead writer", "--topic", "tempfiles", "--type", "epic")

	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dead writer's staged file: stat error = %v, want it removed", err)
	}
	facts := lastEvent(t).Facts
	for key, want := range map[string]any{"temp_table_files.files": float64(1), "temp_table_files.bytes": float64(4), "temp_table_files.error": nil} {
		if facts[key] != want {
			t.Errorf("facts.%s = %v, want %v", key, facts[key], want)
		}
	}
}

// Each failure an open survived is its own warning line, one line however
// many it joins, and an open that never looked for temp table files records
// that as null, not as zero.
func TestOpenWarningsNameEachFailureAnOpenSurvived(t *testing.T) {
	t.Parallel()
	both := storage.Migrated{
		PruneErr:       errors.New("prune migration snapshots: stuck"),
		TempTableFiles: &storage.TempTableFilesCollection{Err: errors.New("read the temp table file dir: denied")},
	}
	want := []string{
		"warning: old migration snapshots or checkpoints were not removed: prune migration snapshots: stuck\n",
		"warning: temp table files a dead writer left were not all removed: read the temp table file dir: denied\n",
	}
	if got := openWarnings(both); !slices.Equal(got, want) {
		t.Errorf("openWarnings() = %q, want %q", got, want)
	}
	joined := storage.Migrated{TempTableFiles: &storage.TempTableFilesCollection{Err: errors.Join(errors.New("remove a: denied"), errors.New("remove b: denied"))}}
	if got, want := openWarnings(joined), []string{"warning: temp table files a dead writer left were not all removed: remove a: denied; remove b: denied\n"}; !slices.Equal(got, want) {
		t.Errorf("openWarnings() of a joined failure = %q, want it on one line: %q", got, want)
	}
	if got := openWarnings(storage.Migrated{TempTableFiles: &storage.TempTableFilesCollection{Files: 2}}); len(got) != 0 {
		t.Errorf("openWarnings() of a clean collection = %q, want none", got)
	}
	facts := storage.Migrated{}.EventRecord()
	for _, key := range []string{"temp_table_files.files", "temp_table_files.bytes", "temp_table_files.error"} {
		if v, ok := facts[key]; !ok || v != nil {
			t.Errorf("an open that never looked records %s = %v (present %v), want null", key, v, ok)
		}
	}
}
