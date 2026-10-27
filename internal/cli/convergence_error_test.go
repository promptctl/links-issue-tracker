package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// convergenceStepReasons is every step of an open's schema convergence and
// the reason its failure prints when its cause names nothing narrower,
// written out by hand: the expected reason must not come from the step table,
// the code under test.
var convergenceStepReasons = map[string]string{
	"classify":                        "migration_failed",
	"verify_content":                  "migration_failed",
	"refuse_unwritable":               "migration_failed",
	"drift_checkpoint":                "migration_failed",
	"drift_repair":                    "migration_failed",
	"drift_repair_reset":              "migration_failed",
	"ensure_quarantine_table":         "migration_failed",
	"quarantine_table_shape":          "schema_shape_refused",
	"commit_quarantine_table":         "migration_failed",
	"check_quarantine":                "migration_failed",
	"check_reconcile_prerequisites":   "migration_failed",
	"reconcile_prerequisites_missing": "schema_shape_refused",
	"snapshot":                        "migration_failed",
	"reconcile":                       "migration_failed",
	"verify_reconciled_shape":         "migration_failed",
	"reconciled_shape_differs":        "schema_shape_refused",
	"adopt_version_table":             "migration_failed",
	"adopt_stamp":                     "migration_partially_applied",
	"adopt_drop_legacy_key":           "migration_failed",
	"commit_adoption_stamp":           "migration_failed",
	"migration_provider":              "migration_failed",
	"migration_checkpoint":            "migration_failed",
	"migration":                       "migration_failed",
	"commit_migration":                "migration_failed",
	"migration_reset":                 "migration_partially_applied",
	"commit_migration_reset":          "migration_failed",
	"record_quarantine":               "migration_failed",
	"commit_quarantine":               "migration_failed",
}

// convergenceStep is the step whose token is token.
func convergenceStep(t *testing.T, token string) store.ConvergenceStep {
	t.Helper()
	for _, step := range store.ConvergenceSteps() {
		if step.String() == token {
			return step
		}
	}
	t.Fatalf("no convergence step %q", token)
	return 0
}

// Every step's failure reads its reason off the step, and none reaches
// command_failed, whose advice to run `lit doctor` cannot help: doctor opens
// the store through the same convergence. A step added without a row here
// fails, so its reason is decided on purpose.
func TestEveryConvergenceStepHasItsOwnReason(t *testing.T) {
	t.Parallel()
	steps := store.ConvergenceSteps()
	if len(steps) != len(convergenceStepReasons) {
		t.Errorf("%d steps, %d rows in convergenceStepReasons", len(steps), len(convergenceStepReasons))
	}
	for _, step := range steps {
		want, ok := convergenceStepReasons[step.String()]
		if !ok {
			t.Errorf("step %q has no row in convergenceStepReasons", step)
			continue
		}
		err := fmt.Errorf("open store: %w", &store.SchemaConvergenceError{Step: step, Cause: errors.New("synthetic")})
		got := commandErrorReason(err)
		if got.String() != want {
			t.Errorf("step %q classifies as %q, want %q", step, got, want)
		}
		if got == reasonCommandFailed {
			t.Errorf("step %q reaches command_failed", step)
		}
	}
}

// A step a retry clears is refined by its cause's own reason; a step that may
// have left partial changes is not, because a contended reset still leaves
// them.
func TestAConvergenceFailureIsRefinedByItsCause(t *testing.T) {
	t.Parallel()
	quarantine := &store.QuarantineBlockError{Version: 9, Name: "00009_x.sql", ErrorText: "simulated failure"}
	for _, tc := range []struct {
		step  string
		cause error
		want  string
	}{
		{"migration", quarantine, "migration_quarantined"},
		{"commit_quarantine", errors.Join(errors.New("commit failed"), quarantine), "migration_quarantined"},
		{"check_quarantine", store.ErrWorkspaceBusy, "workspace_busy"},
		{"snapshot", store.ErrTransientGCContention, "transient_gc_contention"},
		{"commit_migration", store.WorkspaceWriteBlockedError{Cause: errors.New("database is read only")}, "workspace_write_blocked"},
		{"refuse_unwritable", &store.UnsupportedSchemaVersionError{WorkspaceVersion: 99, MaxSupported: 11}, "workspace_schema_ahead"},
		{"migration_reset", store.ErrWorkspaceBusy, "migration_partially_applied"},
		{"adopt_stamp", store.ErrTransientGCContention, "migration_partially_applied"},
	} {
		err := &store.SchemaConvergenceError{Step: convergenceStep(t, tc.step), Cause: tc.cause}
		if got := commandErrorReason(err); got.String() != tc.want {
			t.Errorf("step %q with cause %T classifies as %q, want %q", tc.step, tc.cause, got, tc.want)
		}
	}
}

// A reason carried outside the convergence failure, as the sync open's
// contention stamp carries workspace_busy, holds for a step a retry clears,
// and a step that may have left partial changes still outranks it.
func TestAReasonOutsideAConvergenceFailureStillHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		step string
		want string
	}{
		{"classify", "workspace_busy"},
		{"refuse_unwritable", "workspace_busy"},
		{"migration_reset", "migration_partially_applied"},
		{"quarantine_table_shape", "schema_shape_refused"},
	} {
		fail := &store.SchemaConvergenceError{Step: convergenceStep(t, tc.step), Cause: errors.New("database is locked")}
		err := fmt.Errorf("another process is holding this workspace's Dolt store open: %w (%w)", store.ErrWorkspaceBusy, fail)
		if got := commandErrorReason(err); got.String() != tc.want {
			t.Errorf("step %q under a busy stamp classifies as %q, want %q", tc.step, got, tc.want)
		}
	}
}

// Every reason a convergence failure can reach gives advice that can work:
// it never names `lit doctor`, which opens the store through the same
// convergence; never sends the agent hunting for a process to terminate; and
// never both sends the failure to the user and says it needs no user input.
func TestConvergenceRemediationsNameOnlyActsThatCanWork(t *testing.T) {
	t.Parallel()
	reachable := map[commandReason]bool{
		reasonMigrationQuarantined:  true,
		reasonWorkspaceBusy:         true,
		reasonTransientGcContention: true,
		reasonWorkspaceWriteBlocked: true,
		reasonWorkspaceSchemaAhead:  true,
	}
	for _, step := range store.ConvergenceSteps() {
		reachable[commandErrorReason(&store.SchemaConvergenceError{Step: step, Cause: errors.New("synthetic")})] = true
	}
	for reason := range reachable {
		remediation := commandErrorRemediation(reason)
		for _, banned := range []string{"doctor", "ps aux", "terminate", "kill"} {
			if strings.Contains(remediation, banned) {
				t.Errorf("%s remediation names %q: %s", reason, banned, remediation)
			}
		}
		if strings.Contains(remediation, "to the user") &&
			(strings.Contains(remediation, "without needing the user's input") || strings.Contains(remediation, "without confirmation")) {
			t.Errorf("%s remediation both escalates to the user and says no user input is needed: %s", reason, remediation)
		}
	}
	for reason, want := range map[commandReason]string{
		reasonTransientGcContention: "Retry once: online garbage collection held the store for a moment. If it fails the same way again, surface it to the user as blocking.",
		reasonWorkspaceWriteBlocked: "Wait a moment and retry: a normal command releases the store in well under a second. If it fails the same way again, surface it to the user as blocking.",
	} {
		if got := commandErrorRemediation(reason); got != want {
			t.Errorf("%s remediation = %q, want %q", reason, got, want)
		}
	}
	for reason, want := range map[commandReason][]string{
		reasonMigrationFailed:           {"Retry the command", "from the state it left", "surface it to the user as blocking"},
		reasonMigrationPartiallyApplied: {"Do not retry, and run no other lit command first", "`lit snapshots restore <name>`", "the snapshot the message above names", "moves the current store aside"},
		reasonSchemaShapeRefused:        {"Do not retry unchanged", "The message above names the shape", "surface this refusal to the user as blocking"},
	} {
		got := commandErrorRemediation(reason)
		for _, phrase := range want {
			if !strings.Contains(got, phrase) {
				t.Errorf("%s remediation lacks %q: %s", reason, phrase, got)
			}
		}
	}
}

// A failed convergence records the step, the snapshot, what its copy left out
// and the prune failure under the keys a successful open uses, and the facts
// of the cause it wraps.
func TestAConvergenceFailureRecordsItsStepSnapshotAndCause(t *testing.T) {
	t.Parallel()
	err := &store.SchemaConvergenceError{
		Step:     convergenceStep(t, "refuse_unwritable"),
		Snapshot: &dbsnapshot.Taken{Snapshot: dbsnapshot.Snapshot{Name: "1-pre-migrate-1", Path: "/snapshots/1-pre-migrate-1"}, LeftOut: []string{"links/.dolt/git-remote-cache"}},
		PruneErr: errors.New("prune stuck"),
		Cause:    &store.UnsupportedSchemaVersionError{WorkspaceVersion: 99, MaxSupported: 11},
	}
	end := invocationEnd(time.Now(), fmt.Errorf("open store: %w", err), nil)
	if end.Reason != "workspace_schema_ahead" {
		t.Errorf("reason = %q, want workspace_schema_ahead", end.Reason)
	}
	if got := end.Facts["migration.failed_step"]; got != "refuse_unwritable" {
		t.Errorf("migration.failed_step = %v, want refuse_unwritable", got)
	}
	if got := end.Facts["migration.snapshot"]; got != "1-pre-migrate-1" {
		t.Errorf("migration.snapshot = %v, want the snapshot the open took", got)
	}
	if got, _ := end.Facts["migration.snapshot_left_out"].([]string); !slices.Equal(got, []string{"links/.dolt/git-remote-cache"}) {
		t.Errorf("migration.snapshot_left_out = %v, want what the snapshot's copy left out", end.Facts["migration.snapshot_left_out"])
	}
	if got, _ := end.Facts["migration.prune_error"].(*string); got == nil || *got != "prune stuck" {
		t.Errorf("migration.prune_error = %v, want the prune failure", end.Facts["migration.prune_error"])
	}
	if got := end.Facts["schema_ahead.workspace_schema"]; got != int64(99) {
		t.Errorf("schema_ahead.workspace_schema = %v, want the wrapped refusal's 99", got)
	}
}

// A workspace whose quarantine table is in a shape lit does not converge
// refuses through the CLI at exit 3 with schema_shape_refused, and the event
// names the step.
func TestAnUnconvergeableQuarantineTableRefusesWithItsShapeReason(t *testing.T) {
	isolateEvents(t)
	repo, ws := initBootstrapTestRepo(t)
	info, err := version.Get()
	if err != nil {
		t.Fatalf("version.Get() error = %v", err)
	}
	ctx := context.Background()
	st, err := engine.Open(ctx, testEnv(), testStderr(t), engine.ReadWrite, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	raw, err := storage.TestSupport.Of(st)
	if err != nil {
		t.Fatalf("test support: %v", err)
	}
	// The latest migration pending again, so the open converges, and a
	// quarantine table an older lit left holding a row of history.
	for _, stmt := range []string{
		fmt.Sprintf("DELETE FROM goose_db_version WHERE version_id = %d", info.Schema.Max),
		"DROP TABLE migration_quarantine",
		"CREATE TABLE migration_quarantine (version_id BIGINT NOT NULL, reason TEXT NOT NULL, PRIMARY KEY (version_id))",
		"INSERT INTO migration_quarantine (version_id, reason) VALUES (2, 'old-build failure')",
	} {
		if err := raw.ExecRawForTest(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}

	if _, err := runCLIInDirAllowError(t, repo, "ls"); err == nil {
		t.Fatal("ls succeeded on a workspace whose quarantine table lit does not converge")
	}
	ev := lastEvent(t)
	if ev.ExitCode != ExitValidation || ev.Reason != reasonSchemaShapeRefused.String() {
		t.Errorf("event = exit %d reason %q, want exit %d reason %q", ev.ExitCode, ev.Reason, ExitValidation, reasonSchemaShapeRefused)
	}
	if got := ev.Facts["migration.failed_step"]; got != "quarantine_table_shape" {
		t.Errorf("migration.failed_step = %v, want quarantine_table_shape", got)
	}
}

// A quarantine in force reaches the event as the version it holds, on every
// failure that carries one: the open that recorded it (steps migration and
// commit_quarantine, through the reset outcome that wraps it) and each later
// open it refuses (check_quarantine). Each reads migration_quarantined at exit
// 3, with the remediation that names the quarantine.
func TestAQuarantineInForceIsRecordedOnEveryFailureThatCarriesIt(t *testing.T) {
	t.Parallel()
	quarantine := &store.QuarantineBlockError{Version: 7, Name: "00007_x.sql", ErrorText: "simulated migration failure"}
	for _, tc := range []struct {
		step  string
		cause error
	}{
		{"migration", fmt.Errorf("reset held: %w", quarantine)},
		{"commit_quarantine", errors.Join(errors.New("commit quarantine v7: simulated"), quarantine)},
		{"check_quarantine", quarantine},
	} {
		err := fmt.Errorf("open store: %w", &store.SchemaConvergenceError{Step: convergenceStep(t, tc.step), Cause: tc.cause})
		var stderr strings.Builder
		if exit := WriteCommandError(&stderr, err); exit != 3 || !strings.Contains(stderr.String(), "remediation: Do not retry unchanged — a migration this workspace has pending failed and is quarantined") {
			t.Errorf("step %s: exit %d, stderr %q; want exit 3 and the quarantine remediation", tc.step, exit, stderr.String())
		}
		end := invocationEnd(time.Now(), err, nil)
		if end.Reason != "migration_quarantined" {
			t.Errorf("step %s: reason = %q, want migration_quarantined", tc.step, end.Reason)
		}
		if got := end.Facts["migration.quarantine_version"]; got != int64(7) {
			t.Errorf("step %s: migration.quarantine_version = %v, want 7", tc.step, got)
		}
	}
}
